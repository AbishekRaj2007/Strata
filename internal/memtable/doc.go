// Package memtable buffers recent writes in a sorted in-memory structure.
//
// The concurrent skip list that implements [Memtable] is T3.1 and is not here
// yet; this package currently defines the contract it must satisfy and the
// comparator every ordered structure in Strata shares.
package memtable
