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
	"fmt"

	"github.com/google/gapid/core/log"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/api/vulkan"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/service"
	"github.com/google/gapid/gapis/service/path"
)

type renderpass struct {
	text    string
	consume []uint64
	produce []uint64
}

// GetFramegraph creates the framegraph
func GetFramegraph(ctx context.Context, p *path.Capture) (*service.FramegraphData, error) {
	c, err := capture.ResolveGraphicsFromPath(ctx, p)
	if err != nil {
		return nil, err
	}

	state := c.NewState(ctx)

	renderpasses := []*renderpass{}

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

							fb := ar.Framebuffer()
							fbo := st.Framebuffers().Get(fb)

							fbWidth := fbo.Width()
							fbHeight := fbo.Height()

							fbImgs := fbo.ImageAttachments()

							rpID := rpo.VulkanHandle()

							text := fmt.Sprintf("RP: %v [%v x %v])", rpID, fbWidth, fbHeight)
							consume := []uint64{}
							produce := []uint64{}

							for i := uint32(0); i < uint32(rpo.AttachmentDescriptions().Len()); i++ {
								if i >= uint32(fbImgs.Len()) {
									break
								}
								attachment := rpo.AttachmentDescriptions().Get(i)
								attImg := fbImgs.Get(i).Image().VulkanHandle()
								if attachment.LoadOp() == vulkan.VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD {
									log.W(ctx, "HUGUES %v consume: %v", rpID, attImg)
									consume = append(consume, uint64(attImg))
								}
								if attachment.StoreOp() == vulkan.VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_STORE {
									log.W(ctx, "HUGUES %v produce: %v", rpID, attImg)
									produce = append(produce, uint64(attImg))
								}

							}

							renderpasses = append(renderpasses, &renderpass{
								text:    text,
								consume: consume,
								produce: produce,
							})

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

	// Construct graph

	imgProd := map[uint64]int{}
	imgCons := map[uint64]int{}

	nodes := []*service.FramegraphNode{}

	for i, rp := range renderpasses {
		for _, img := range rp.consume {
			imgCons[img] = i
		}
		for _, img := range rp.produce {
			imgProd[img] = i
		}
		nodes = append(nodes, &service.FramegraphNode{
			Id:   uint64(i),
			Text: rp.text,
		})
	}

	edges := []*service.FramegraphEdge{}

	for img, rpCons := range imgCons {
		if rpProd, ok := imgProd[img]; ok {
			edges = append(edges, &service.FramegraphEdge{
				Origin:      uint64(rpProd),
				Destination: uint64(rpCons),
			})
		}
	}

	return &service.FramegraphData{Nodes: nodes, Edges: edges}, nil
}
