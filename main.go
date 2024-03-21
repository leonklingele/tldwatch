package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag" //nolint:depguard // We only allow to import the flag package in here
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/idna"

	_ "modernc.org/sqlite"
)

const (
	requestTimeout = 10 * time.Second

	tldURL = "https://data.iana.org/TLD/tlds-alpha-by-domain.txt"
)

const (
	defaultSQLiteFilePath = "./db.sqlite"

	sqliteInitStmt = `
		begin;
		create table tlds (
			tld text primary key not null
		) strict;
		commit;
	`
	sqliteInsertStmt = `
		insert into tlds (tld) values (?);
	`
)

//nolint:gochecknoglobals // Nice to use as a global
var logTarget = os.Stderr

type tld string

func loadTLDs(ctx context.Context, requestTimeout time.Duration, l *slog.Logger) ([]tld, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tldURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	res, err := (&http.Client{
		Timeout: requestTimeout,
	}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			l.Error(fmt.Errorf("failed to close body: %w", err).Error())
		}
	}()

	prof := idna.New(idna.BidiRule())

	var tlds []tld
	for scanner := bufio.NewScanner(res.Body); scanner.Scan(); {
		line := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		t, err := prof.ToUnicode(line)
		if err != nil {
			l.Error(fmt.Errorf("failed to puny decode %q: %w", line, err).Error())
		}

		tlds = append(tlds, tld(t))
	}

	return tlds, nil
}

func run(
	ctx context.Context,
	l *slog.Logger,
	sqliteFile string,
) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	tlds, err := loadTLDs(ctx, requestTimeout, l)
	if err != nil {
		return err
	}

	var isFirstRun bool
	if _, err := os.Stat(sqliteFile); os.IsNotExist(err) {
		isFirstRun = true
	}

	db, err := sql.Open("sqlite", sqliteFile)
	if err != nil {
		return fmt.Errorf("failed to open sqlite database: %w", err)
	}

	if isFirstRun {
		if _, err := db.Exec(sqliteInitStmt); err != nil {
			return fmt.Errorf("failed to init database: %w", err)
		}
		l.Info("successfully initialized database")
	}

	stmt, err := db.Prepare(sqliteInsertStmt)
	if err != nil {
		return fmt.Errorf("failed to prepare insert statement: %w", err)
	}
	defer func() {
		if err := stmt.Close(); err != nil {
			l.Error(fmt.Errorf("failed to close insert statement: %w", err).Error())
		}
	}()

	newTLDs := make([]tld, 0, len(tlds))
	for _, tld := range tlds {
		if _, err := stmt.ExecContext(
			context.WithoutCancel(ctx),
			tld,
		); err != nil {
			// TODO: Properly check for error, see https://gitlab.com/cznic/sqlite/-/blob/f49aba7eddcec7d31797e72c67aafb0398970730/all_test.go#L2228
			if got, want := err.Error(), "constraint failed: UNIQUE constraint failed: tlds.tld (1555)"; got == want {
				// This is fine
				continue
			}

			l.Error(
				"failed to exec insert statement",
				"err", err,
				"tld", fmt.Sprintf("%+v", tld),
			)
			continue
		}

		newTLDs = append(newTLDs, tld)
	}

	// Print as JSON
	if err := json.NewEncoder(os.Stdout).Encode(newTLDs); err != nil {
		return fmt.Errorf("failed to JSON-print to stdout: %w", err)
	}

	return nil
}

func main() {
	debug := flag.Bool("debug", false, "enable debug mode")

	flag.Parse()

	sqliteFile := getenv("SQLITE_FILE", defaultSQLiteFilePath)

	ll := new(slog.LevelVar)
	ll.Set(slog.LevelInfo)
	l := slog.New(slog.NewJSONHandler(logTarget, &slog.HandlerOptions{
		Level: ll,
	}))
	slog.SetDefault(l)

	// We have a debug env var as well as a debug CLI flag
	if getenv("DEBUG", "false") == "true" {
		*debug = true
	}

	if *debug {
		ll.Set(slog.LevelDebug)
	}

	if err := run(
		context.Background(),
		l,
		sqliteFile,
	); err != nil {
		l.Error(err.Error())
	}
}
