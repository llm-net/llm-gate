package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestArchivesAreDeterministicAndExecutable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "gate-source")
	want := []byte("fixture-binary\n")
	if err := os.WriteFile(src, want, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"tar", "zip"} {
		t.Run(format, func(t *testing.T) {
			one := filepath.Join(dir, format+"-one")
			two := filepath.Join(dir, format+"-two")
			var err error
			if format == "tar" {
				err = writeTarGZ(one, src, "gate")
				if err == nil {
					err = writeTarGZ(two, src, "gate")
				}
			} else {
				err = writeZip(one, src, "gate.exe")
				if err == nil {
					err = writeZip(two, src, "gate.exe")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			b1, _ := os.ReadFile(one)
			b2, _ := os.ReadFile(two)
			if !bytes.Equal(b1, b2) {
				t.Fatal("same input produced different archives")
			}
			got, mode := readArchiveFixture(t, format, b1)
			if !bytes.Equal(got, want) || mode&0o111 == 0 {
				t.Fatalf("content=%q mode=%o", got, mode)
			}
		})
	}
}

func readArchiveFixture(t *testing.T, format string, body []byte) ([]byte, os.FileMode) {
	t.Helper()
	if format == "tar" {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if _, err := out.ReadFrom(tr); err != nil {
			t.Fatal(err)
		}
		return out.Bytes(), os.FileMode(h.Mode)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("zip: files=%d", len(zr.File))
	}
	r, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), zr.File[0].Mode()
}
