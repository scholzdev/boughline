package boughline

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

const backupFormat = "boughline-backup-v1\n"

var backupColumns = map[string][]string{
	"bough_blob_claims": {"repo", "principal", "id"},
	"bough_repos":       {"id", "config"}, "bough_changes": {"repo", "id", "created_at"}, "bough_requests": {"repo", "key", "fingerprint", "revision"}, "bough_derived": {"repo", "revision", "kind", "object"}, "bough_refs": {"repo", "name", "target"}, "bough_tokens": {"hash", "record"}, "bough_grants": {"repo", "principal", "record"}, "bough_reviews": {"repo", "revision", "reviewer", "record"},
}
var backupTables = []string{"bough_repos", "bough_blob_claims", "bough_changes", "bough_requests", "bough_derived", "bough_refs", "bough_tokens", "bough_grants", "bough_reviews"}

// Backup takes one consistent SQL snapshot, then archives verified immutable
// objects into the same independent artifact. No source-bucket dependency remains.
func Backup(ctx context.Context, db *SQLMetadata, store *S3Store, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fail(409, "backup destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(filepath.Dir(destination), ".bough-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, table := range backupTables {
		file, err := os.OpenFile(filepath.Join(scratch, table), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		columns := backupColumns[table]
		selection := []string{}
		for _, column := range columns {
			selection = append(selection, column+"::TEXT")
		}
		rows, err := tx.Query(ctx, "SELECT "+strings.Join(selection, ",")+" FROM "+table)
		if err != nil {
			file.Close()
			return err
		}
		writer := bufio.NewWriter(file)
		for rows.Next() {
			values := make([]string, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err = rows.Scan(targets...); err != nil {
				break
			}
			if _, err = writer.Write(append(Encode(values), '\n')); err != nil {
				break
			}
		}

		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err == nil {
			err = writer.Flush()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(scratch, "backup.tar"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	archive := tar.NewWriter(file)
	add := func(name string, reader io.Reader, size int64) error {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.CopyN(archive, reader, size)
		return err
	}
	if err = add("format", strings.NewReader(backupFormat), int64(len(backupFormat))); err != nil {
		return err
	}
	for _, table := range backupTables {
		metadata, err := os.Open(filepath.Join(scratch, table))
		if err != nil {
			return err
		}
		info, err := metadata.Stat()
		if err == nil {
			err = add("metadata/"+table, metadata, info.Size())
		}
		metadata.Close()
		if err != nil {
			return err
		}
	}
	for object := range store.Client.ListObjects(ctx, store.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			return object.Err
		}
		parts := strings.Split(object.Key, "/")
		if len(parts) != 3 {
			return fail(500, "invalid stored object key")
		}
		kind, id := parts[0], parts[1]+parts[2]
		packed, err := store.Get(ctx, kind, id)
		if err != nil {
			return err
		}
		if _, err = Unpack(packed, id); err != nil {
			return err
		}
		if err = add("objects/"+kind+"/"+id, strings.NewReader(string(packed)), int64(len(packed))); err != nil {
			return err
		}
	}
	if err = archive.Close(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Link(filepath.Join(scratch, "backup.tar"), destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

// Restore requires empty native tables and an empty independent bucket. Objects
// are durable before metadata is published in one transaction. A failed restore
// can leave harmless objects; it never overwrites an existing repository.
func Restore(ctx context.Context, db *SQLMetadata, store *S3Store, source string) error {
	for object := range store.Client.ListObjects(ctx, store.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			return object.Err
		}
		return fail(409, "restore requires an empty object bucket")
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	scratch, err := os.MkdirTemp(filepath.Dir(source), ".bough-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	reader := tar.NewReader(file)
	header, err := reader.Next()
	if err != nil {
		return err
	}
	if header.Name != "format" || header.Size != int64(len(backupFormat)) {
		return fail(400, "not a Boughline backup")
	}
	format, err := io.ReadAll(reader)
	if err != nil || string(format) != backupFormat {
		return fail(400, "unsupported backup format")
	}
	seen := map[string]bool{}
	for {
		header, err = reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 {
			return fail(400, "invalid backup entry")
		}
		if strings.HasPrefix(header.Name, "metadata/") {
			table := strings.TrimPrefix(header.Name, "metadata/")
			valid := false
			for _, expected := range backupTables {
				if table == expected {
					valid = true
					break
				}
			}
			if !valid || seen[table] {
				return fail(400, "unknown or duplicate backup metadata")
			}
			seen[table] = true
			out, err := os.OpenFile(filepath.Join(scratch, table), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = io.CopyN(out, reader, header.Size)
			closeErr := out.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		} else if strings.HasPrefix(header.Name, "objects/") {
			if header.Size > MaxBlob+(1<<20) {
				return fail(413, "backup object exceeds limit")
			}
			kind, id, ok := strings.Cut(strings.TrimPrefix(header.Name, "objects/"), "/")
			if !ok {
				return fail(400, "invalid backup object")
			}
			packed, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil {
				return err
			}
			if int64(len(packed)) != header.Size {
				return fail(400, "truncated backup object")
			}
			if err = store.Put(ctx, kind, id, packed); err != nil {
				return err
			}
		} else {
			return fail(400, "unknown backup entry")
		}
	}
	for _, table := range backupTables {
		if !seen[table] {
			return fail(400, "backup is missing metadata", table)
		}
	}
	// Check every reachable source and derived object before publishing metadata.
	seeds := []objectRef{}
	for _, table := range []string{"bough_blob_claims", "bough_changes", "bough_derived"} {
		metadata, err := os.Open(filepath.Join(scratch, table))
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(metadata)
		scanner.Buffer(make([]byte, 4096), MaxRequest)
		for scanner.Scan() {
			var row []string
			if err = json.Unmarshal(scanner.Bytes(), &row); err != nil || len(row) != len(backupColumns[table]) {
				metadata.Close()
				return fail(400, "invalid backup metadata")
			}
			if table == "bough_blob_claims" {
				seeds = append(seeds, objectRef{"blob", row[2]})
			} else if table == "bough_changes" {
				seeds = append(seeds, objectRef{"changeset", row[1]})
			} else {
				kind, err := derivedObjectKind(row[2])
				if err != nil {
					metadata.Close()
					return err
				}
				seeds = append(seeds, objectRef{kind, row[3]})
			}
		}
		err = scanner.Err()
		metadata.Close()
		if err != nil {
			return err
		}
	}
	if _, err = verifyObjects(ctx, store, seeds); err != nil {
		return err
	}
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, table := range backupTables {
		var count int64
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fail(409, "restore requires empty native tables", table)
		}
	}
	for _, table := range backupTables {
		metadata, err := os.Open(filepath.Join(scratch, table))
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(metadata)
		scanner.Buffer(make([]byte, 4096), MaxRequest)
		columns := backupColumns[table]
		placeholders := []string{}
		for i, column := range columns {
			cast := ""
			if column == "config" || column == "record" {
				cast = "::JSONB"
			}
			if column == "created_at" {
				cast = "::TIMESTAMPTZ"
			}
			placeholders = append(placeholders, fmt.Sprintf("$%d::TEXT%s", i+1, cast))
		}
		statement := "INSERT INTO " + table + " (" + strings.Join(columns, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
		for scanner.Scan() {
			var values []string
			if err = json.Unmarshal(scanner.Bytes(), &values); err != nil || len(values) != len(columns) {
				metadata.Close()
				return fail(400, "invalid backup metadata")
			}
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			if _, err = tx.Exec(ctx, statement, args...); err != nil {
				metadata.Close()
				return fmt.Errorf("restore %s: %w", table, err)
			}
		}

		err = scanner.Err()
		metadata.Close()
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
