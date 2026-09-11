package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	MaxFileSizeMB = 4
	MaxFileSize   = MaxFileSizeMB << 20 // 4MB

	MaxPartialFileSizeMB = 1
	MaxPartialFileSize   = MaxPartialFileSizeMB << 20 // 1 MiB

	MaxBatchFiles = 50
)

var (
	ErrFileTooLarge      = errors.New("file too large")
	ErrReadLimitExceeded = errors.New("read limit exceeded")
)

type ToolReadFileI struct {
	Path string `json:"path"`
}

type ToolReadFileO struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
}

type ToolReadFile struct {
	fs *FileSystem
}

var _ tools.Tool = ToolReadFile{}

func (t ToolReadFile) Name() string {
	return "read_file"
}

func (t ToolReadFile) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolReadFileI,
) (*mcp.CallToolResult, ToolReadFileO, error) {
	f, err := t.fs.root.Open(in.Path)
	if err != nil {
		return nil, ToolReadFileO{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, ToolReadFileO{}, err
	}

	if info.IsDir() {
		return nil, ToolReadFileO{},
			fmt.Errorf("%q is a directory", in.Path)
	}

	if info.Size() > MaxFileSize {
		return nil, ToolReadFileO{},
			fmt.Errorf("%w: maximum size is %d MB",
				ErrFileTooLarge,
				MaxFileSizeMB,
			)
	}

	bs, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, ToolReadFileO{}, err
	}
	if len(bs) > MaxFileSize {
		return nil, ToolReadFileO{},
			fmt.Errorf("%w: maximum size is %d MB",
				ErrFileTooLarge,
				MaxFileSizeMB,
			)
	}

	if !utf8.Valid(bs) {
		return nil, ToolReadFileO{},
			fmt.Errorf("%q is not valid UTF-8", in.Path)
	}

	content := string(bs)
	return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: content,
				},
			},
		}, ToolReadFileO{
			Path:    in.Path,
			Content: content,
			Size:    int64(len(bs)),
		}, nil
}

func (t ToolReadFile) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Read a UTF-8 text file from the current workspace.",
		},
		t.handle,
	)
	return nil
}

type ToolReadPartialFileI struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type ToolReadPartialFileO struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
}

type ToolReadPartialFile struct {
	fs *FileSystem
}

var _ tools.Tool = ToolReadPartialFile{}

func (t ToolReadPartialFile) Name() string {
	return "read_partial_file"
}

func (t ToolReadPartialFile) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolReadPartialFileI,
) (*mcp.CallToolResult, ToolReadPartialFileO, error) {
	if in.StartLine < 1 {
		return nil, ToolReadPartialFileO{},
			errors.New("start_line must be greater than zero")
	}

	if in.EndLine < in.StartLine {
		return nil, ToolReadPartialFileO{},
			errors.New("end_line must be greater than or equal to start_line")
	}

	f, err := t.fs.root.Open(in.Path)
	if err != nil {
		return nil, ToolReadPartialFileO{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, ToolReadPartialFileO{}, err
	}

	if info.IsDir() {
		return nil, ToolReadPartialFileO{},
			fmt.Errorf("%q is a directory", in.Path)
	}

	reader := NewLimitedReader(f, LimitedReaderOptions{
		Separator:     DefaultSeparator,
		MaxLineBytes:  DefaultMaxLineBytes,
		MaxTotalBytes: MaxPartialFileSize,
	})
	res, err := reader.ReadLines(in.StartLine, in.EndLine)
	if err != nil {
		if strings.Contains(err.Error(), "not valid UTF-8") {
			return nil, ToolReadPartialFileO{},
				fmt.Errorf("%q is not valid UTF-8", in.Path)
		}
		return nil, ToolReadPartialFileO{}, err
	}

	out := ToolReadPartialFileO{
		Path:      in.Path,
		StartLine: in.StartLine,
		EndLine:   res.EffectiveEnd,
		Content:   res.Content,
		Size:      int64(len(res.Content)),
	}
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: res.Content,
			},
		},
	}
	if res.StopReason == StopReadLimitExceeded || res.StopReason == StopLineTooLong {
		return result, out, errors.New(strings.Join(res.Details, "; "))
	}
	return result, out, nil
}

func (t ToolReadPartialFile) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Read a UTF-8 text file from the current workspace. Prefer small line ranges; if the range hits a limit, retry with a narrower range or fewer lines.",
		},
		t.handle,
	)
	return nil
}
