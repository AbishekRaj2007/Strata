// Package engine presents the storage engine's public key-value operations.
//
// The write path is split across two files. Rotation (rotation.go) owns the
// active memtable, the WAL that protects it, and the bounded queue of
// immutable memtables awaiting flush. [Flusher] (flush.go) consumes that
// queue: it builds an SSTable, commits it to the manifest, and only then
// retires the memtable and its WAL.
//
// That ordering is the engine's durability argument. The manifest fsync is
// the commit point, and the WAL may only be deleted once its data is durable
// *and* referenced by a durable manifest -- never merely once the SSTable
// itself has been fsynced.
package engine
