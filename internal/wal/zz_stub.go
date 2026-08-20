package wal

import "os"

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

func (f FragmentType) String() string { return "" }

type Kind uint8

const (
	KindSet    Kind = 0
	KindDelete Kind = 1
)

type Record struct {
	Kind  Kind
	Key   []byte
	Value []byte
}

type Batch struct {
	Sequence uint64
	Records  []Record
}

func (b *Batch) AppendSet(key, value []byte) {}
func (b *Batch) AppendDelete(key []byte)     {}
func (b *Batch) Encode(dst []byte) []byte    { return nil }

func DecodeBatch(payload []byte) (Batch, error) { return Batch{}, nil }

var ErrCorrupt = os.ErrInvalid

type Writer struct{}

func NewWriter(f *os.File) *Writer                    { return nil }
func (w *Writer) Write(b *Batch) (int64, error)       { return 0, nil }
func (w *Writer) Offset() int64                       { return 0 }
func (w *Writer) Sync() error                         { return nil }
func (w *Writer) Close() error                        { return nil }

type Reader struct{}

func NewReader(f *os.File) *Reader      { return nil }
func (r *Reader) Next() (Batch, error)  { return Batch{}, nil }

func Recover(dir string, apply func(seq uint64, rec Record) error) (uint64, error) { return 0, nil }
