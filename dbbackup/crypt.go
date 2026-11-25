package dbbackup

import (
	"compress/gzip"
	"fmt"
	"io"

	"filippo.io/age"
)

type encWriter struct {
	gz  *gzip.Writer
	enc io.WriteCloser
}

func (e *encWriter) Write(p []byte) (int, error) { return e.gz.Write(p) }

// Close finalizes the gzip stream and then the age stream (not w).
func (e *encWriter) Close() error {
	gerr := e.gz.Close()
	if err := e.enc.Close(); err != nil {
		return err
	}
	return gerr
}

// Encrypt returns a writer that gzips what is written and encrypts it to all
// recipients, writing to w. Close finalizes both layers.
func Encrypt(w io.Writer, recipients []age.Recipient) (io.WriteCloser, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no age recipients")
	}
	enc, err := age.Encrypt(w, recipients...)
	if err != nil {
		return nil, err
	}
	return &encWriter{gz: gzip.NewWriter(enc), enc: enc}, nil
}

// Decrypt reverses Encrypt: age decrypt, then gunzip.
func Decrypt(r io.Reader, identities []age.Identity) (io.Reader, error) {
	dr, err := age.Decrypt(r, identities...)
	if err != nil {
		return nil, err
	}
	return gzip.NewReader(dr)
}
