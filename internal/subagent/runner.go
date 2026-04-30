package subagent

import (
	"context"

	"github.com/MMinasyan/lightcode/internal/loop"
)

func Run(ctx context.Context, lp *loop.Loop, prompt string) (string, error) {
	return lp.Run(ctx, prompt)
}
