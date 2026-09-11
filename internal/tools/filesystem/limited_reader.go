package filesystem

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	DefaultSeparator     byte = '\n'
	DefaultMaxLineBytes       = 64 * 1024
	DefaultMaxTotalBytes      = MaxPartialFileSize
)

var ErrLineTooLong = errors.New("line too long")

type StopReason string

const (
	StopEOF               StopReason = "EOF"
	StopRangeComplete     StopReason = "RANGE_COMPLETE"
	StopReadLimitExceeded StopReason = "READ_LIMIT_EXCEEDED"
	StopLineTooLong       StopReason = "LINE_TOO_LONG"
)

type ReadResult struct {
	Content      string
	EffectiveEnd int
	LinesScanned int
	StopReason   StopReason
	Details      []string
}

type LimitedReaderOptions struct {
	Separator     byte
	MaxLineBytes  int
	MaxTotalBytes int
}

func (o LimitedReaderOptions) normalize() (sep byte, maxLine int, maxTotal int) {
	sep = o.Separator
	if sep == 0 {
		sep = DefaultSeparator
	}
	maxLine = o.MaxLineBytes
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}
	maxTotal = o.MaxTotalBytes
	if maxTotal <= 0 {
		maxTotal = DefaultMaxTotalBytes
	}
	return sep, maxLine, maxTotal
}

type LimitedReader struct {
	br       *bufio.Reader
	sep      byte
	maxLine  int
	maxTotal int
}

func NewLimitedReader(r io.Reader, opts LimitedReaderOptions) *LimitedReader {
	sep, maxLine, maxTotal := opts.normalize()
	return &LimitedReader{
		br:       bufio.NewReader(r),
		sep:      sep,
		maxLine:  maxLine,
		maxTotal: maxTotal,
	}
}

func lineTooLongDetail(n, cap int) string {
	return fmt.Sprintf("line %d exceeds per-line limit %d bytes; narrow the line range or read fewer lines", n, cap)
}

func readLimitDetail(m, collected, cap int) string {
	return fmt.Sprintf("collecting line %d would exceed total limit %d bytes (%d collected); narrow the line range or read fewer lines", m, cap, collected)
}

func (lr *LimitedReader) ReadLines(start, end int) (ReadResult, error) {
	if start < 1 {
		return ReadResult{}, errors.New("start_line must be greater than zero")
	}
	if end < start {
		return ReadResult{}, errors.New("end_line must be greater than or equal to start_line")
	}

	var sb strings.Builder
	cur := 0
	lastAppended := start - 1

	limitLine := func(n int) (ReadResult, error) {
		return ReadResult{
			Content:      sb.String(),
			EffectiveEnd: lastAppended,
			LinesScanned: n,
			StopReason:   StopLineTooLong,
			Details:      []string{lineTooLongDetail(n, lr.maxLine)},
		}, nil
	}

	limitTotal := func(m int) (ReadResult, error) {
		return ReadResult{
			Content:      sb.String(),
			EffectiveEnd: lastAppended,
			LinesScanned: m,
			StopReason:   StopReadLimitExceeded,
			Details:      []string{readLimitDetail(m, sb.Len(), lr.maxTotal)},
		}, nil
	}

	var lineBuf []byte

	// processLine handles one complete line (including sep except trailing).
	// Returns (stop bool, result, err): stop=true means return result immediately.
	processLine := func(line []byte) (bool, ReadResult, error) {
		cur++
		if len(line) > lr.maxLine {
			res, _ := limitLine(cur)
			return true, res, nil
		}
		if cur < start || cur > end {
			return false, ReadResult{}, nil
		}
		if !utf8.Valid(line) {
			return true, ReadResult{}, fmt.Errorf("line %d is not valid UTF-8", cur)
		}
		if sb.Len()+len(line) > lr.maxTotal {
			res, _ := limitTotal(cur)
			return true, res, nil
		}
		sb.Write(line)
		lastAppended = cur
		return false, ReadResult{}, nil
	}

	for {
		b, rerr := lr.br.ReadByte()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				if len(lineBuf) > 0 {
					cp := slices.Clone(lineBuf)
					lineBuf = lineBuf[:0]
					if stop, res, err := processLine(cp); err != nil {
						return ReadResult{}, err
					} else if stop {
						return res, nil
					}
				}
				break
			}
			return ReadResult{}, rerr
		}
		lineBuf = append(lineBuf, b)
		if len(lineBuf) > lr.maxLine {
			return limitLine(cur + 1)
		}
		if b == lr.sep {
			cp := slices.Clone(lineBuf)
			lineBuf = lineBuf[:0]
			if stop, res, err := processLine(cp); err != nil {
				return ReadResult{}, err
			} else if stop {
				return res, nil
			}
			if cur >= end {
				if _, perr := lr.br.Peek(1); perr != nil {
					if !errors.Is(perr, io.EOF) {
						return ReadResult{}, perr
					}
					return ReadResult{
						Content:      sb.String(),
						EffectiveEnd: min(end, cur),
						LinesScanned: cur,
						StopReason:   StopEOF,
					}, nil
				}
				return ReadResult{
					Content:      sb.String(),
					EffectiveEnd: min(end, cur),
					LinesScanned: cur,
					StopReason:   StopRangeComplete,
				}, nil
			}
		}
	}

	if cur < start {
		return ReadResult{}, fmt.Errorf("start_line %d exceeds file length", start)
	}
	return ReadResult{
		Content:      sb.String(),
		EffectiveEnd: min(end, cur),
		LinesScanned: cur,
		StopReason:   StopEOF,
	}, nil
}
