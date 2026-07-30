package utils

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const payload = "the quick brown fox jumps over the lazy dog"

func gzipCompress(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func brotliCompress(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func deflateCompress(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func zstdCompress(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func TestDecompressResponse(t *testing.T) {
	tests := []struct {
		encoding string
		data     []byte
	}{
		{"gzip", gzipCompress(t, payload)},
		{"br", brotliCompress(t, payload)},
		{"deflate", deflateCompress(t, payload)},
		{"zstd", zstdCompress(t, payload)},
	}

	for _, tt := range tests {
		t.Run(tt.encoding, func(t *testing.T) {
			got, err := DecompressResponse(tt.encoding, tt.data)
			if err != nil {
				t.Fatalf("DecompressResponse() error: %v", err)
			}
			if string(got) != payload {
				t.Errorf("DecompressResponse() = %q, want %q", got, payload)
			}
		})
	}
}

func TestDecompressResponsePassthrough(t *testing.T) {
	raw := []byte("plain body")

	tests := []struct {
		name     string
		encoding string
		data     []byte
		want     []byte
	}{
		{"no encoding", "", raw, raw},
		{"empty data", "gzip", []byte{}, []byte{}},
		{"unknown encoding", "lzma", raw, raw},
		{"corrupt gzip data returns original", "gzip", raw, raw},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecompressResponse(tt.encoding, tt.data)
			if err != nil {
				t.Fatalf("DecompressResponse() error: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("DecompressResponse() = %q, want %q", got, tt.want)
			}
		})
	}
}

type failingDecompressor struct{}

func (failingDecompressor) Decompress([]byte) ([]byte, error) { return nil, errors.New("boom") }

func TestRegisterDecompressor(t *testing.T) {
	original, existed := decompressorRegistry["custom-test"]
	t.Cleanup(func() {
		if existed {
			decompressorRegistry["custom-test"] = original
		} else {
			delete(decompressorRegistry, "custom-test")
		}
	})

	RegisterDecompressor("custom-test", failingDecompressor{})

	// A failing decompressor degrades gracefully to the original data.
	got, err := DecompressResponse("custom-test", []byte("data"))
	if err != nil {
		t.Fatalf("DecompressResponse() error: %v", err)
	}
	if string(got) != "data" {
		t.Errorf("DecompressResponse() = %q, want original data", got)
	}
}

func TestDecompressorsRejectInvalidData(t *testing.T) {
	invalid := []byte("definitely not compressed")

	decompressors := map[string]Decompressor{
		"gzip":    &GzipDecompressor{},
		"deflate": &DeflateDecompressor{},
		"zstd":    &ZstdDecompressor{},
		"br":      &BrotliDecompressor{},
	}

	for name, d := range decompressors {
		t.Run(name, func(t *testing.T) {
			if _, err := d.Decompress(invalid); err == nil {
				t.Errorf("%s Decompress() expected error on invalid data", name)
			}
		})
	}
}
