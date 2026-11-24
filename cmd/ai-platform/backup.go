package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/tr1gl1de/kindevai/internal/platform/backup"
)

// exportCmd writes a consistent archive of the installation (KIN-41). It reads the data directory directly (the
// trusted local channel, like setup-token) and works while the Control Plane runs.
func exportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("export", flag.ContinueOnError)
	fset.SetOutput(stderr)
	dataDir := fset.String("data-dir", defaultDataDir(), "data directory of the installation")
	keyFile := fset.String("key-file", "", "the installation's key file (default <data-dir>/secrets/master.key)")
	out := fset.String("out", "", "archive to write (required; must not exist)")
	keyOut := fset.String("key-out", "", "also write the encryption key to this separate file (0600; must not exist)")
	withoutKey := fset.Bool("without-key", false, "do not export the key: you will copy it separately")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("export: --out is required")
	}
	m, err := backup.Export(ctx, backup.ExportOptions{DataDir: *dataDir, KeyFile: *keyFile, Out: *out, KeyOut: *keyOut,
		WithoutKey: *withoutKey, AppVersion: version})
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	fmt.Fprintf(stdout, "exported installation %s (schema %d, %d files) to %s\n", m.InstallationID, m.SchemaVersion,
		len(m.Files), *out)
	if *keyOut != "" {
		fmt.Fprintf(stderr, "WARNING: %s is the encryption key. Anyone holding it and the archive can read every "+
			"stored secret. Keep it apart from the archive (another medium or channel) and delete copies you no longer need.\n",
			*keyOut)
	} else {
		fmt.Fprintln(stderr, "WARNING: the encryption key is not in the archive. Import needs it: copy the installation's "+
			"key file separately and securely, or the stored secrets cannot be restored.")
	}
	return nil
}

// importCmd restores an archive into an empty data directory (KIN-41).
func importCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("import", flag.ContinueOnError)
	fset.SetOutput(stderr)
	dataDir := fset.String("data-dir", defaultDataDir(), "empty data directory to restore into (created if missing)")
	keyFile := fset.String("key-file", "", "where the installation keeps its key (default <data-dir>/secrets/master.key)")
	archive := fset.String("archive", "", "archive written by ai-platform export (required)")
	key := fset.String("key", "", "the encryption key file exported with the archive (required)")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if *archive == "" {
		return fmt.Errorf("import: --archive is required")
	}
	m, err := backup.Import(ctx, backup.ImportOptions{Archive: *archive, Key: *key, DataDir: *dataDir, KeyFile: *keyFile,
		AppVersion: version})
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	fmt.Fprintf(stdout, "imported installation %s (exported %s by %s, schema %d) into %s\n", m.InstallationID,
		m.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), m.AppVersion, m.SchemaVersion, *dataDir)
	fmt.Fprintln(stdout, "start the Control Plane with `ai-platform serve`; runners reconnect by themselves. If the "+
		"export was taken in maintenance mode, switch it off in Settings once you have checked the installation.")
	return nil
}
