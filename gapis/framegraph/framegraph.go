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

const (
	kindRenderpass = iota
	kindCompute
)

type workload struct {
	kind  int
	text  string
	read  map[uint64]bool
	write map[uint64]bool
}

// GetFramegraph creates the framegraph
func GetFramegraph(ctx context.Context, p *path.Capture) (*service.FramegraphData, error) {
	c, err := capture.ResolveGraphicsFromPath(ctx, p)
	if err != nil {
		return nil, err
	}

	state := c.NewState(ctx)

	workloads := []*workload{}

	mutate := func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		err := cmd.Mutate(ctx, id, state, nil, nil)
		if err != nil {
			return err
		}

		// API-specific things start here.

		// Assume:
		// - single queue
		// - renderpasses only, in order
		// - only interact with images
		var currentRP *workload = nil
		currentReadableImg := map[uint64]bool{}
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
							// BeginRenderPass has info about framebuffer and attachments
							rp := ar.RenderPass()
							rpo := st.RenderPasses().Get(rp)
							rpID := rpo.VulkanHandle()

							fb := ar.Framebuffer()
							fbo := st.Framebuffers().Get(fb)

							fbWidth := fbo.Width()
							fbHeight := fbo.Height()

							fbImgs := fbo.ImageAttachments()

							text := fmt.Sprintf("RP:%x [%vx%v]", rpID, fbWidth, fbHeight)
							consume := map[uint64]bool{}
							produce := map[uint64]bool{}

							for i := uint32(0); i < uint32(rpo.AttachmentDescriptions().Len()); i++ {
								if i >= uint32(fbImgs.Len()) {
									break
								}
								attachment := rpo.AttachmentDescriptions().Get(i)
								attImg := fbImgs.Get(i).Image().VulkanHandle()
								if attachment.LoadOp() == vulkan.VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD {
									log.W(ctx, "HUGUES %v consume: %v", rpID, attImg)
									consume[uint64(attImg)] = true
								}
								if attachment.StoreOp() == vulkan.VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_STORE {
									log.W(ctx, "HUGUES %v produce: %v", rpID, attImg)
									produce[uint64(attImg)] = true
								}

							}

							currentRP = &workload{
								kind:  kindRenderpass,
								text:  text,
								read:  consume,
								write: produce,
							}

							workloads = append(workloads, currentRP)

						case vulkan.VkCmdBindDescriptorSetsArgsʳ:

							currentReadableImg = map[uint64]bool{}
							descriptorSets := ar.DescriptorSets()

							for i := 0; i < descriptorSets.Len(); i++ {
								dsIdx := descriptorSets.Get(uint32(i))
								ds := st.DescriptorSets().Get(dsIdx)
								log.W(ctx, "HUGUES descrset: %v", ds)

								dsBindings := ds.Bindings()
								log.W(ctx, "HUGUES bindings: %v, len: %v", dsBindings, dsBindings.Len())

								for j := 0; j < dsBindings.Len(); j++ {
									dsb := dsBindings.Get(uint32(j))
									log.W(ctx, "HUGUES dsb: %v type: %v", dsb, dsb.BindingType())
									if dsb.BindingType() == vulkan.VkDescriptorType_VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER {
										imgBindings := dsb.ImageBinding()
										log.W(ctx, "HUGUES imgBinding: %v len: %v", imgBindings, imgBindings.Len())
										for k := 0; k < imgBindings.Len(); k++ {
											dii := imgBindings.Get(uint32(k))
											imgViewIdx := dii.ImageView()
											log.W(ctx, "HUGUES dii: %v imgView: %v", dii, imgViewIdx)
											imgView := st.ImageViews().Get(imgViewIdx)
											imgObj := imgView.Image()
											imgHandle := imgObj.VulkanHandle()
											log.W(ctx, "HUGUES READ img: %v", imgHandle)
											currentReadableImg[uint64(imgHandle)] = true
										}
									}
								}
							}

						case vulkan.VkCmdDrawIndexedArgsʳ:
							log.W(ctx, "HUGUES drawcmdindexed, indexcount: %v", ar.IndexCount())
							for k := range currentReadableImg {
								log.W(ctx, "HUGUES DRAW reads: %v", k)
								currentRP.read[k] = true
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

	// Update node labels with read/write info
	for _, w := range workloads {
		w.text += "\\nRead: "
		for k := range w.read {
			w.text += fmt.Sprintf(" %x", k)
		}
		w.text += "\\nWrite:"
		for k := range w.write {
			w.text += fmt.Sprintf(" %x", k)
		}
	}

	// Construct graph

	imgProd := map[uint64]int{}
	imgCons := map[uint64]int{}

	nodes := []*service.FramegraphNode{}

	for i, w := range workloads {
		for img := range w.read {
			imgCons[img] = i
		}
		for img := range w.write {
			imgProd[img] = i
		}
		nodes = append(nodes, &service.FramegraphNode{
			Id:   uint64(i),
			Text: w.text,
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
