/*
 * MinIO Cloud Storage, (C) 2017-2020 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package ioutil implements some I/O utility functions which are not covered
// by the standard library.
package ioutil

import (
	"bytes"
	"io"
	"os"

	"github.com/minio/minio/pkg/disk"
)

// WriteOnCloser implements io.WriteCloser and always
// executes at least one write operation if it is closed.
//
// This can be useful within the context of HTTP. At least
// one write operation must happen to send the HTTP headers
// to the peer.
type WriteOnCloser struct {
	io.Writer
	hasWritten bool
}

func (w *WriteOnCloser) Write(p []byte) (int, error) {
	w.hasWritten = true
	return w.Writer.Write(p)
}

// Close closes the WriteOnCloser. It behaves like io.Closer.
func (w *WriteOnCloser) Close() error {
	if !w.hasWritten {
		_, err := w.Write(nil)
		if err != nil {
			return err
		}
	}
	if closer, ok := w.Writer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// HasWritten returns true if at least one write operation was performed.
func (w *WriteOnCloser) HasWritten() bool { return w.hasWritten }

// WriteOnClose takes an io.Writer and returns an ioutil.WriteOnCloser.
func WriteOnClose(w io.Writer) *WriteOnCloser {
	return &WriteOnCloser{w, false}
}

// LimitWriter implements io.WriteCloser.
//
// This is implemented such that we want to restrict
// an enscapsulated writer upto a certain length
// and skip a certain number of bytes.
type LimitWriter struct {
	io.Writer
	skipBytes int64
	wLimit    int64
}

// Write implements the io.Writer interface limiting upto
// configured length, also skips the first N bytes.
func (w *LimitWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	var n1 int
	if w.skipBytes > 0 {
		if w.skipBytes >= int64(len(p)) {
			w.skipBytes = w.skipBytes - int64(len(p))
			return n, nil
		}
		p = p[w.skipBytes:]
		w.skipBytes = 0
	}
	if w.wLimit == 0 {
		return n, nil
	}
	if w.wLimit < int64(len(p)) {
		n1, err = w.Writer.Write(p[:w.wLimit])
		w.wLimit = w.wLimit - int64(n1)
		return n, err
	}
	n1, err = w.Writer.Write(p)
	w.wLimit = w.wLimit - int64(n1)
	return n, err
}

// Close closes the LimitWriter. It behaves like io.Closer.
func (w *LimitWriter) Close() error {
	if closer, ok := w.Writer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// LimitedWriter takes an io.Writer and returns an ioutil.LimitWriter.
func LimitedWriter(w io.Writer, skipBytes int64, limit int64) *LimitWriter {
	return &LimitWriter{w, skipBytes, limit}
}

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

// NopCloser returns a WriteCloser with a no-op Close method wrapping
// the provided Writer w.
func NopCloser(w io.Writer) io.WriteCloser {
	return nopCloser{w}
}

// SkipReader skips a given number of bytes and then returns all
// remaining data.
type SkipReader struct {
	io.Reader

	skipCount int64
}

func (s *SkipReader) Read(p []byte) (int, error) {
	l := int64(len(p))
	if l == 0 {
		return 0, nil
	}
	for s.skipCount > 0 {
		if l > s.skipCount {
			l = s.skipCount
		}
		n, err := s.Reader.Read(p[:l])
		if err != nil {
			return 0, err
		}
		s.skipCount -= int64(n)
	}
	return s.Reader.Read(p)
}

// NewSkipReader - creates a SkipReader
func NewSkipReader(r io.Reader, n int64) io.Reader {
	return &SkipReader{r, n}
}

// SameFile returns if the files are same.
func SameFile(fi1, fi2 os.FileInfo) bool {
	if !os.SameFile(fi1, fi2) {
		return false
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		return false
	}
	if fi1.Mode() != fi2.Mode() {
		return false
	}
	if fi1.Size() != fi2.Size() {
		return false
	}
	return true
}

// DirectIO alignment needs to be 4K. Defined here as
// directio.AlignSize is defined as 0 in MacOS causing divide by 0 error.
const directioAlignSize = 4096

// CopyAligned - copies from reader to writer using the aligned input
// buffer, it is expected that input buffer is page aligned to
// 4K page boundaries. Without passing aligned buffer may cause
// this function to return error.
//
// This code is similar in spirit to io.Copy but it is only to be
// used with DIRECT I/O based file descriptor and it is expected that
// input writer *os.File not a generic io.Writer. Make sure to have
// the file opened for writes with syscall.O_DIRECT flag.
func CopyAligned(w *os.File, r io.Reader, alignedBuf []byte, totalSize int64) (int64, error) {
	// Writes remaining bytes in the buffer.
	writeUnaligned := func(w *os.File, buf []byte) (remainingWritten int64, err error) {
		// Disable O_DIRECT on fd's on unaligned buffer
		// perform an amortized Fdatasync(fd) on the fd at
		// the end, this is performed by the caller before
		// closing 'w'.
		if err = disk.DisableDirectIO(w); err != nil {
			return remainingWritten, err
		}
		return io.Copy(w, bytes.NewReader(buf))
	}

	var written int64
	for {
		buf := alignedBuf
		if totalSize != -1 {
			remaining := totalSize - written
			if remaining < int64(len(buf)) {
				buf = buf[:remaining]
			}
		}
		nr, err := io.ReadFull(r, buf)
		eof := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !eof {
			return written, err
		}
		buf = buf[:nr]
		var nw int64
		if len(buf)%directioAlignSize == 0 {
			var n int
			// buf is aligned for directio write()
			n, err = w.Write(buf)
			nw = int64(n)
		} else {
			// buf is not aligned, hence use writeUnaligned()
			nw, err = writeUnaligned(w, buf)
		}
		if nw > 0 {
			written += nw
		}
		if err != nil {
			return written, err
		}
		if nw != int64(len(buf)) {
			return written, io.ErrShortWrite
		}

		if totalSize != -1 {
			if written == totalSize {
				return written, nil
			}
		}
		if eof {
			return written, nil
		}
	}
}
