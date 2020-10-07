// Copyright (C) 2020 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resolve

import (
	"context"
	"strings"

	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/service"
	"github.com/google/gapid/gapis/service/path"
)

// GetFramegraph creates the framegraph
func GetFramegraph(ctx context.Context, p *path.Capture) (*service.FramegraphData, error) {
	c, err := capture.ResolveGraphicsFromPath(ctx, p)
	if err != nil {
		return nil, err
	}

	state := c.NewState(ctx)

	nodes := []*service.FramegraphNode{}
	nodeId := uint64(0)

	mutate := func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		err := cmd.Mutate(ctx, id, state, nil, nil)
		if err != nil {
			return err
		}
		// TODO use if _, ok := cmd.(*vkQueueSubmit), but we have a Bazel dependency issue when
		// importing gapis/api/vulkan?
		if strings.Contains(cmd.CmdName(), "Draw") {
			nodes = append(nodes, &service.FramegraphNode{Id: nodeId, Text: cmd.CmdName()})
			nodeId++
		}
		return nil
	}

	err = api.ForeachCmd(ctx, c.Commands, true, mutate)
	if err != nil {
		return nil, err
	}

	return &service.FramegraphData{Nodes: nodes}, nil
}
