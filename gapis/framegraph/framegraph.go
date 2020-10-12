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

package framegraph

import (
	"context"

	"github.com/google/gapid/core/log"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/api/vulkan"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/service"
	"github.com/google/gapid/gapis/service/path"
)

// type rp {
// 	consume []uint64
// 	produce []uint64
// }

// GetFramegraph creates the framegraph
func GetFramegraph(ctx context.Context, p *path.Capture) (*service.FramegraphData, error) {
	c, err := capture.ResolveGraphicsFromPath(ctx, p)
	if err != nil {
		return nil, err
	}

	state := c.NewState(ctx)

	nodes := []*service.FramegraphNode{}
	nodeId := uint64(0)

	//rps := []*rp{}

	mutate := func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		err := cmd.Mutate(ctx, id, state, nil, nil)
		if err != nil {
			return err
		}

		if queueSubmit, ok := cmd.(*vulkan.VkQueueSubmit); ok {
			st := vulkan.GetState(state)
			layout := state.MemoryLayout
			pSubmits := queueSubmit.PSubmits().Slice(0, uint64(queueSubmit.SubmitCount()), layout).MustRead(ctx, queueSubmit, state, nil)
			for _, submit := range pSubmits {
				commandBuffers := submit.PCommandBuffers().Slice(0, uint64(submit.CommandBufferCount()), layout).MustRead(ctx, queueSubmit, state, nil)
				for _, cb := range commandBuffers {
					commandBuffer := st.CommandBuffers().Get(cb)
					for i := 0; i < commandBuffer.CommandReferences().Len(); i++ {
						cr := commandBuffer.CommandReferences().Get(uint32(i))
						args := vulkan.GetCommandArgs(ctx, cr, st)
						switch ar := args.(type) {
						case vulkan.VkCmdBeginRenderPassArgsʳ:
							rp := ar.RenderPass()
							rpo := st.RenderPasses().Get(rp)
							log.W(ctx, "HUGUES renderpassobject: %v", rpo)
							for i := uint32(0); i < uint32(rpo.AttachmentDescriptions().Len()); i++ {
								attachment := rpo.AttachmentDescriptions().Get(i)
								log.W(ctx, "HUGUES attachment loadop: %v", attachment.LoadOp())
								log.W(ctx, "HUGUES attachment storeop: %v", attachment.StoreOp())

								nodes = append(nodes, &service.FramegraphNode{Id: nodeId, Text: "att"})
								nodeId++

							}
						}
					}
				}
			}
		}

		return nil
	}

	err = api.ForeachCmd(ctx, c.Commands, true, mutate)
	if err != nil {
		return nil, err
	}

	return &service.FramegraphData{Nodes: nodes}, nil
}
