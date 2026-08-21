// Command goredis is the second half of the T1.3 done-when condition: an
// ordinary go-redis program, written the way anyone would write it against
// real Redis, running unmodified against strata-server.
//
// It lives outside the Strata module on purpose. go-redis is not on the
// permitted dependency list in CLAUDE.md, and a verification program is not a
// reason to put it there.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/redis/go-redis/v9"
)

var (
	pass int
	fail int
)

func check(what string, want, got any) {
	w, g := fmt.Sprint(want), fmt.Sprint(got)
	if w == g {
		fmt.Printf("  ok    %-32s -> %s\n", what, g)
		pass++
		return
	}
	fmt.Printf("  FAIL  %-32s -> got [%s] want [%s]\n", what, g, w)
	fail++
}

func main() {
	addr := "localhost:6390"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()

	fmt.Println("== connection ==")
	pong, err := rdb.Ping(ctx).Result()
	if err != nil {
		fmt.Printf("go-redis could not reach %s: %v\n", addr, err)
		os.Exit(1)
	}
	check("Ping", "PONG", pong)
	check("Echo", "strata", must(rdb.Echo(ctx, "strata").Result()))

	fmt.Println("== string round trip ==")
	rdb.FlushDB(ctx)
	check("Set", "OK", must(rdb.Set(ctx, "k", "v", 0).Result()))
	check("Get", "v", must(rdb.Get(ctx, "k").Result()))
	check("Set overwrite", "OK", must(rdb.Set(ctx, "k", "v2", 0).Result()))
	check("Get after overwrite", "v2", must(rdb.Get(ctx, "k").Result()))

	// The idiomatic absent-key check. A client that does not get redis.Nil
	// here cannot tell "missing" from "error", which is the whole reason the
	// engine interface returns (value, error) rather than (value, bool).
	_, err = rdb.Get(ctx, "definitely-absent").Result()
	check("Get absent is redis.Nil", true, errors.Is(err, redis.Nil))

	fmt.Println("== binary safety ==")
	blob := []byte{0x00, 0x01, 0xff, 0xfe, '\r', '\n', 0x7f}
	check("Set binary", "OK", must(rdb.Set(ctx, "bin", blob, 0).Result()))
	got, err := rdb.Get(ctx, "bin").Bytes()
	check("Get binary error", nil, err)
	check("Get binary round trip", fmt.Sprintf("%v", blob), fmt.Sprintf("%v", got))

	fmt.Println("== exists / del ==")
	rdb.FlushDB(ctx)
	rdb.Set(ctx, "a", "1", 0)
	rdb.Set(ctx, "b", "2", 0)
	rdb.Set(ctx, "c", "3", 0)
	check("Exists one", int64(1), must(rdb.Exists(ctx, "a").Result()))
	check("Exists absent", int64(0), must(rdb.Exists(ctx, "zzz").Result()))
	check("Exists variadic", int64(3), must(rdb.Exists(ctx, "a", "b", "c").Result()))
	check("Del one", int64(1), must(rdb.Del(ctx, "a").Result()))
	check("Del absent", int64(0), must(rdb.Del(ctx, "a").Result()))
	check("Del variadic", int64(2), must(rdb.Del(ctx, "b", "c").Result()))

	fmt.Println("== dbsize / flushdb ==")
	rdb.FlushDB(ctx)
	check("DBSize empty", int64(0), must(rdb.DBSize(ctx).Result()))
	rdb.Set(ctx, "x", "1", 0)
	rdb.Set(ctx, "y", "2", 0)
	check("DBSize after 2 sets", int64(2), must(rdb.DBSize(ctx).Result()))
	check("FlushDB", "OK", must(rdb.FlushDB(ctx).Result()))
	check("DBSize after flush", int64(0), must(rdb.DBSize(ctx).Result()))

	fmt.Println("== scan ==")
	rdb.FlushDB(ctx)
	for i := 0; i < 20; i++ {
		rdb.Set(ctx, fmt.Sprintf("key:%02d", i), "v", 0)
	}
	rdb.Set(ctx, "other:1", "v", 0)

	keys, cursor, err := rdb.Scan(ctx, 0, "key:*", 100).Result()
	check("Scan MATCH error", nil, err)
	sort.Strings(keys)
	check("Scan MATCH count", 20, len(keys))
	check("Scan MATCH first", "key:00", keys[0])
	check("Scan returns a cursor", true, cursor == 0 || cursor > 0)

	// Iterator is how most real code scans. It drives the cursor internally,
	// so it is the honest test of cursor semantics: a cursor that does not
	// advance hangs here, and one that skips loses keys.
	iter := rdb.Scan(ctx, 0, "", 3).Iterator()
	seen := map[string]bool{}
	for iter.Next(ctx) {
		seen[iter.Val()] = true
	}
	check("Iterator error", nil, iter.Err())
	check("Iterator visited every key", 21, len(seen))

	fmt.Println("== server commands ==")
	info, err := rdb.Info(ctx).Result()
	check("Info error", nil, err)
	check("Info non-empty", true, len(info) > 0)
	check("Do COMPACT", "OK", must(rdb.Do(ctx, "COMPACT").Result()))

	fmt.Println("== error handling ==")
	// A wrong-arity command must come back as a RESP error the client surfaces
	// as an error, not as a value.
	_, err = rdb.Do(ctx, "GET").Result()
	check("Wrong arity is an error", true, err != nil && !errors.Is(err, redis.Nil))
	_, err = rdb.Do(ctx, "NOSUCHCMD").Result()
	check("Unknown command is an error", true, err != nil && !errors.Is(err, redis.Nil))

	// The connection must still be usable after two protocol-level errors. If
	// replies desynchronise, this is where it shows.
	check("Usable after errors", "PONG", must(rdb.Ping(ctx).Result()))

	fmt.Println("== pipeline ==")
	rdb.FlushDB(ctx)
	pipe := rdb.Pipeline()
	setCmd := pipe.Set(ctx, "p1", "one", 0)
	getCmd := pipe.Get(ctx, "p1")
	sizeCmd := pipe.DBSize(ctx)
	_, err = pipe.Exec(ctx)
	check("Pipeline exec error", nil, err)
	check("Pipeline set", "OK", must(setCmd.Result()))
	check("Pipeline get", "one", must(getCmd.Result()))
	check("Pipeline dbsize", int64(1), must(sizeCmd.Result()))

	fmt.Printf("\npassed=%d failed=%d\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

func must[T any](v T, err error) any {
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	return v
}
