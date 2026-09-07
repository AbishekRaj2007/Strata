package engine

import (
	"fmt"
	"testing"
)

// TestEveryFlushInterleavingOfWriteOverwriteDelete is T4.3's done-when.
//
// The operation sequence is fixed: write a key, overwrite it five times, then
// delete it. What varies is where the flushes fall. Each of the seven
// operations may or may not be followed by a flush, so there are 2^7 = 128
// distinct interleavings, and every one must end with the key reported
// absent.
//
// Exhaustive rather than random because the space is small enough to cover
// completely, and because the failures here are structural -- a version
// landing in the wrong level, a tombstone losing to an older value, an L0
// ordering mistake -- so they hide in specific interleavings rather than
// showing up under load. A random sample would probably find them; an
// exhaustive sweep proves there is nothing left.
func TestEveryFlushInterleavingOfWriteOverwriteDelete(t *testing.T) {
	const (
		key = "the-key"
		ops = 7 // 1 write + 5 overwrites + 1 delete
	)

	for mask := 0; mask < 1<<ops; mask++ {
		t.Run(fmt.Sprintf("flushmask=%03b", mask), func(t *testing.T) {
			// A large threshold means rotation only happens when this test
			// asks for it, so the mask alone decides the interleaving.
			env := newFlushEnv(t, 1<<30, 8)

			step := func(i int, do func()) {
				do()
				if mask&(1<<i) != 0 {
					// Force the active memtable into the immutable queue and
					// flush it, which is what a rotation plus a flush does.
					if err := env.set.rotate(); err != nil {
						t.Fatalf("rotate: %v", err)
					}
					if err := env.f.DrainQueue(); err != nil {
						t.Fatalf("DrainQueue: %v", err)
					}
				}
			}

			step(0, func() { env.put(t, key, "v0") })
			for i := 1; i <= 5; i++ {
				v := fmt.Sprintf("v%d", i)
				step(i, func() { env.put(t, key, v) })
			}
			step(6, func() { env.del(t, key) })

			// Whatever the interleaving, the key is deleted.
			e, found, err := lookup(env.set, env.vs.Current(), &dirTables{dir: env.dir}, []byte(key))
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if !found {
				// No version at all is also a correct "absent", but it should
				// not happen here: the tombstone is a version and must be
				// findable.
				t.Fatal("no version found at all; the tombstone was lost")
			}
			if !e.Tombstone {
				t.Fatalf("lookup returned %+v, want a tombstone -- a deleted key resurfaced with value %q", e, e.Value)
			}
		})
	}
}

// TestEveryFlushInterleavingKeepsTheNewestValue is the companion property.
// The same sweep without the trailing delete must always return the last
// value written, never an earlier one.
func TestEveryFlushInterleavingKeepsTheNewestValue(t *testing.T) {
	const (
		key = "the-key"
		ops = 6 // 1 write + 5 overwrites
	)

	for mask := 0; mask < 1<<ops; mask++ {
		t.Run(fmt.Sprintf("flushmask=%02b", mask), func(t *testing.T) {
			env := newFlushEnv(t, 1<<30, 8)

			step := func(i int, do func()) {
				do()
				if mask&(1<<i) != 0 {
					if err := env.set.rotate(); err != nil {
						t.Fatalf("rotate: %v", err)
					}
					if err := env.f.DrainQueue(); err != nil {
						t.Fatalf("DrainQueue: %v", err)
					}
				}
			}

			step(0, func() { env.put(t, key, "v0") })
			for i := 1; i <= 5; i++ {
				v := fmt.Sprintf("v%d", i)
				step(i, func() { env.put(t, key, v) })
			}

			e, found, err := lookup(env.set, env.vs.Current(), &dirTables{dir: env.dir}, []byte(key))
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if !found {
				t.Fatal("key not found after six writes")
			}
			if e.Tombstone {
				t.Fatal("got a tombstone for a key that was never deleted")
			}
			if string(e.Value) != "v5" {
				t.Fatalf("value = %q, want \"v5\" -- an older version won", e.Value)
			}
		})
	}
}
