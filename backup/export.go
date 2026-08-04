package backup

import (
	"context"
	"crypto/rand"
	"cs-agent/backup/borg"
	"cs-agent/s3upload"
	"cs-agent/store"
	"cs-agent/types"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/spf13/viper"
)

// exportDetailFailed labels the failure-detail line in the task result output.
const exportDetailFailed = "agent-3f8b2c1e7a9d4056"

// exportArchiveSuffix returns the object-key extension for an export, derived
// from the configured borg --tar-filter so the published filename matches the
// actual bytes (e.g. a gzip filter yields ".tar.gz"). It reads the SAME viper
// key that borg.ExportTar uses to apply the filter, so the two can't drift.
// The filter may carry flags ("gzip -9"), so we key off the command word only;
// an unrecognized filter falls back to ".tar".
func exportArchiveSuffix() string {
	filter := strings.TrimSpace(viper.GetString("backups.export.tar_filter"))
	if filter == "" {
		return ".tar"
	}
	switch strings.Fields(filter)[0] {
	case "gzip", "pigz":
		return ".tar.gz"
	case "zstd":
		return ".tar.zst"
	case "bzip2", "pbzip2":
		return ".tar.bz2"
	case "xz":
		return ".tar.xz"
	case "lz4":
		return ".tar.lz4"
	default:
		return ".tar"
	}
}

var objectKeyUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// ExportBackup streams a chosen archive to S3 and, on success, records the
// presigned GET URL + size/expiry in the task result (result_json) for the
// controller to read — there is no separate KV download record anymore. The task
// status (completed/failed) is the readiness gate.
//
// Runs under the per-repo lock so a compact never rewrites segments mid-stream.
// The borg export uses --bypass-lock, so scheduled backups (create) are never
// blocked. A presigned URL is published ONLY when both the borg export exited 0
// and the upload succeeded. A crashed export is left "running" by the worker and
// failed by the boot crash-reconcile — never blindly re-run.
func ExportBackup(ctx context.Context, st *store.Store, task store.Task, projectEvent *progress) error {
	// No handler-level sentry.Recover(): let a panic reach the worker terminal
	// guard so a crashed export is FAILED (never a false "completed").
	v, found, err := st.GetVolume(ctx, task.Volume)
	if err != nil {
		backupLogger().Warn("Export: error loading volume from store", "volume", task.Volume, "error", err.Error())
		sentry.CaptureException(err)
		return err
	}
	if !found {
		return failExport(projectEvent, "unknown volume")
	}
	vol, err := types.LoadVolume(v.Config)
	if err != nil {
		backupLogger().Warn("Export: error parsing volume", "volume", task.Volume, "error", err.Error())
		sentry.CaptureException(err)
		return err
	}

	params := parseParams(task)

	// S3 destination (credentials are node-local config, never from the task).
	s3cfg := s3upload.ConfigFromViper()
	if !s3cfg.Enabled() {
		return failExport(projectEvent, "backup export is not configured (no S3 bucket)")
	}
	if cfgErr := s3cfg.Validate(0); cfgErr != nil {
		return failExport(projectEvent, cfgErr.Error())
	}
	uploader, err := s3upload.New(s3cfg)
	if err != nil {
		return failExport(projectEvent, "s3 init: "+err.Error())
	}

	// Bound the whole export so a hung borg or a stalled S3 endpoint can't hold
	// the per-repo lock or the export worker forever.
	if t := viper.GetInt("backups.export.timeout_sec"); t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
		defer cancel()
	}

	// Serialize against compact/prune of the same repo for the whole stream.
	//
	// vol.Name is the right key because an export is a same-volume operation: it is passed
	// as both arguments to FindRepository below, so it IS the repository owner the lock
	// names. It cannot be taken from repo.Name instead — the repository does not exist yet,
	// and opening it first would race the container build this lock exists to serialize.
	defer borg.AcquireRepoLock(vol.Name)()

	repo, findErr := borg.FindRepository(st, &vol, &vol)
	if findErr != nil {
		return failExport(projectEvent, "find repository: "+findErr.Message)
	}
	archive, archErr := repo.FindArchive(task.Archive)
	if archErr != nil {
		repo.StopContainer()
		return failExport(projectEvent, "find archive: "+archErr.Message)
	}

	objectKey := task.ID + "/" + randomToken() + "/" +
		sanitizeKeySegment(vol.Name) + "-" + sanitizeKeySegment(task.Archive) + exportArchiveSuffix()

	// Stream: borg export-tar (producer) -> io.Pipe -> S3 multipart upload.
	pr, pw := io.Pipe()
	exportErrCh := make(chan *borg.LogMessage, 1)
	go func() {
		// n5: recover a panic in the producer so it can't crash the whole agent.
		// Convert it into a pipe error + a channel message so the parent unblocks
		// and the task fails cleanly (the enclosing ExportBackup's defer can't
		// protect a separate goroutine).
		defer func() {
			if r := recover(); r != nil {
				hub := sentry.CurrentHub().Clone()
				hub.Recover(r)
				hub.Flush(2 * time.Second)
				msg := fmt.Sprintf("export producer panicked: %v", r)
				backupLogger().Error("export producer panicked", "archive", task.Archive, "panic", msg)
				pw.CloseWithError(errors.New(msg))
				exportErrCh <- &borg.LogMessage{Message: msg}
			}
		}()
		lg := archive.ExportTar(ctx, pw)
		if lg != nil {
			// Make the uploader's Read fail so it can't Complete a truncated tar.
			pw.CloseWithError(errors.New(lg.Message))
		} else {
			pw.Close()
		}
		exportErrCh <- lg
	}()

	size, upErr := uploader.Upload(ctx, objectKey, pr)

	// If the upload abandoned the read (error or timeout), unblock the producer's
	// pw.Write so the export goroutine can't leak.
	pr.CloseWithError(upErr)

	// Stream is drained; safe to tear the container down now (not via an early defer).
	repo.StopContainer()

	exportLog := <-exportErrCh // synchronizes with the producer goroutine

	// Publish a URL ONLY if borg exited 0 AND the upload succeeded.
	if exportLog != nil {
		return failExport(projectEvent, "export collided with a concurrent repo write or borg failed (retry): "+exportLog.Message)
	}
	if upErr != nil {
		return failExport(projectEvent, "upload failed: "+upErr.Error())
	}

	url, expiry, psErr := uploader.PresignGet(ctx, objectKey, time.Duration(params.DownloadTTL)*time.Second)
	if psErr != nil {
		return failExport(projectEvent, "presign failed: "+psErr.Error())
	}

	projectEvent.Set("url", url)
	projectEvent.Set("object_key", s3cfg.Prefix+objectKey)
	projectEvent.Set("size", size)
	projectEvent.Set("expiry", expiry.Unix())
	backupLogger().Info("Completed backup export", "volume", vol.Name, "archive", task.Archive, "size", size)
	return nil
}

func failExport(p *progress, msg string) error {
	backupLogger().Warn("Backup export failed", "error", msg)
	p.EventLog.Status = "failed"
	p.PostEventUpdate(exportDetailFailed, msg)
	return errors.New(msg)
}

func randomToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is effectively impossible; fall back to a timestamp.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	return hex.EncodeToString(b)
}

func sanitizeKeySegment(s string) string {
	return objectKeyUnsafe.ReplaceAllString(s, "_")
}
