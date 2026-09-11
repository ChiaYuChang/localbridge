package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

type Tool interface {
	Name() string
	Register(s *mcp.Server) error
}
