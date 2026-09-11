package teams

import (
	"context"
	orchestration "github.com/anthropics/agent-manager/orchestrator"
)

func Run(ctx context.Context, cfg Config, group string, port int, task string, mcp *MCPServer) error {
	loop := NewLoop(cfg, group)
	loop.SetLogFunc(mcp.logFunc)
	loop.SetDoneCh(mcp.DoneCh())
	loop.SetTaskCh(mcp.TaskCh())
	mcp.SetStateFunc(func() string { return loop.State().String() })
	mcp.SetPauseFunc(loop.Pause)
	mcp.SetResumeFunc(loop.Resume)
	return orchestration.Serve(ctx, port, mcp.handler(), func(ctx context.Context) error { return loop.Run(ctx, task) })
}
