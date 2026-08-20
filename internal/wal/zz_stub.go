package wal

import "os"

// Compile-only placeholders for the parts of T2.1 and T2.3 that are not
// written yet, so the test suite builds and fails with named assertions
// rather than build errors. Delete each declaration as the real one lands;
// delete the file when Recover works.

const (
	BlockSize      = 32768
	HeaderSize     = 7
	MaxPayloadSize = BlockSize - HeaderSize
)

type FragmentType uint8

const (
	FragmentFull   FragmentType = 1
	FragmentFirst  FragmentType = 2
	FragmentMiddle FragmentType = 3
	FragmentLast   FragmentType = 4
)

type Writer struct{}

func NewWriter(f *os.File) *Writer              { return nil }
func (w *Writer) Write(b *Batch) (int64, error) { return 0, nil }
func (w *Writer) Offset() int64                 { return 0 }
func (w *Writer) Sync() error                   { return nil }
func (w *Writer) Close() error                  { return nil }

type Reader struct{}

func NewReader(f *os.File) *Reader     { return nil }
func (r *Reader) Next() (Batch, error) { return Batch{}, nil }

func Recover(dir string, apply func(seq uint64, rec Record) error) (uint64, error) { return 0, nil }
