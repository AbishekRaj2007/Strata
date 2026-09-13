package vfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOSCreateIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")

	f, err := OS().Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Exclusive creation is what makes a never-reused file number a
	// guarantee rather than a convention, so a second Create must fail.
	if _, err := OS().Create(path); !errors.Is(err, os.ErrExist) {
		t.Errorf("second Create returned %v, want ErrExist", err)
	}

	// CreateTruncate is the one that is allowed to replace.
	g, err := OS().CreateTruncate(path)
	if err != nil {
		t.Fatalf("CreateTruncate: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSyncDirClosesOnFailure(t *testing.T) {
	dir := t.TempDir()

	if err := SyncDir(nil, dir); err != nil {
		t.Fatalf("SyncDir on the real filesystem: %v", err)
	}

	in := NewInjector(nil)
	in.ArmAt(1, Fault{}) // 0 is the OpenDir, 1 is the Sync
	err := SyncDir(in, dir)
	if !errors.Is(err, ErrInjected) {
		t.Fatalf("SyncDir returned %v, want the injected error", err)
	}
	if rec, ok := in.Fired(); !ok || rec.Kind != OpSync {
		t.Errorf("fault landed on %v (fired=%v), want a sync", rec, ok)
	}
}

func TestInjectorCountsEveryOperation(t *testing.T) {
	dir := t.TempDir()
	in := NewInjector(nil)
	in.Trace(true)

	path := filepath.Join(dir, "f")
	f, err := in.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	trace := in.Trace(true)
	want := []OpKind{OpCreate, OpWrite, OpSync, OpClose}
	if len(trace) != len(want) {
		t.Fatalf("trace is %v, want %d entries", trace, len(want))
	}
	for i, kind := range want {
		if trace[i].Kind != kind {
			t.Errorf("trace[%d] is %s, want %s", i, trace[i].Kind, kind)
		}
		if trace[i].Index != int64(i) {
			t.Errorf("trace[%d] has index %d", i, trace[i].Index)
		}
	}
	if got := in.Count(); got != int64(len(want)) {
		t.Errorf("Count is %d, want %d", got, len(want))
	}
}

func TestInjectorFiresExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	in := NewInjector(nil)

	f, err := in.Create(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer f.Close()

	in.ArmAt(in.Count(), Fault{})

	if _, err := f.Write([]byte("a")); !errors.Is(err, ErrInjected) {
		t.Fatalf("first write returned %v, want the injected error", err)
	}
	// A fault that re-fires is a different scenario -- a permanently broken
	// file rather than one failed operation -- and not the one being swept.
	if _, err := f.Write([]byte("a")); err != nil {
		t.Fatalf("second write returned %v, want success", err)
	}
}

func TestInjectorModes(t *testing.T) {
	dir := t.TempDir()

	t.Run("torn write lands half the bytes", func(t *testing.T) {
		path := filepath.Join(dir, "torn")
		in := NewInjector(nil)
		f, err := in.Create(path)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		in.ArmAt(in.Count(), Fault{Mode: ModeTorn})

		payload := []byte("0123456789")
		n, err := f.Write(payload)
		if !errors.Is(err, ErrInjected) {
			t.Fatalf("write returned %v, want the injected error", err)
		}
		if n != len(payload)/2 {
			t.Errorf("write reported %d bytes, want %d", n, len(payload)/2)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "01234" {
			t.Errorf("file holds %q, want the first half", got)
		}
	})

	t.Run("short read returns fewer bytes", func(t *testing.T) {
		path := filepath.Join(dir, "short")
		if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}

		in := NewInjector(nil)
		f, err := in.Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer f.Close()

		in.ArmAt(in.Count(), Fault{Mode: ModeShort})
		buf := make([]byte, 10)
		n, err := f.Read(buf)
		if err != nil {
			t.Fatalf("Read returned %v, want a short read with no error", err)
		}
		if n != 5 {
			t.Errorf("Read returned %d bytes, want 5", n)
		}

		// ReadAt must report an error when it comes up short, unlike Read.
		in.ArmAt(in.Count(), Fault{Mode: ModeShort})
		n, err = f.ReadAt(buf, 0)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("short ReadAt returned %v, want ErrUnexpectedEOF", err)
		}
		if n != 5 {
			t.Errorf("short ReadAt returned %d bytes, want 5", n)
		}
	})

	t.Run("latency delays and succeeds", func(t *testing.T) {
		in := NewInjector(nil)
		in.ArmAt(0, Fault{Mode: ModeLatency, Delay: 20 * time.Millisecond})

		start := time.Now()
		f, err := in.Create(filepath.Join(dir, "slow"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		defer f.Close()
		if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
			t.Errorf("Create took %s, want the injected delay", elapsed)
		}
	})

	t.Run("a specific errno survives", func(t *testing.T) {
		in := NewInjector(nil)
		in.ArmAt(0, Fault{Err: os.ErrNotExist})
		if _, err := in.Open(filepath.Join(dir, "any")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Open returned %v, want the injected ErrNotExist", err)
		}
	})
}

func TestInjectorPassesThroughWhenUnarmed(t *testing.T) {
	dir := t.TempDir()
	in := NewInjector(nil)

	if err := in.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "sub", "f")
	f, err := in.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write([]byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := in.ReadFile(path)
	if err != nil || string(data) != "payload" {
		t.Fatalf("ReadFile returned %q, %v", data, err)
	}
	if _, err := in.Stat(path); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	entries, err := in.ReadDir(filepath.Join(dir, "sub"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir returned %d entries, %v", len(entries), err)
	}
	if err := in.Rename(path, path+".moved"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := in.Remove(path + ".moved"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, fired := in.Fired(); fired {
		t.Error("an unarmed injector fired")
	}
}

func TestOrDefaultsToTheRealFilesystem(t *testing.T) {
	if Or(nil) != Default {
		t.Error("Or(nil) is not the real filesystem")
	}
	in := NewInjector(nil)
	if Or(in) != FS(in) {
		t.Error("Or replaced a supplied filesystem")
	}
}
