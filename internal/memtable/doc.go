// Package memtable buffers recent writes in a sorted in-memory structure.
//
// [SkipList] is the concurrent skip list that implements [Memtable]: T3.1's
// contract, and the comparator every ordered structure in Strata shares.
package memtable
