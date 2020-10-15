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

// resource kind
const (
	resourceImage = iota
	resourceBuffer
)

func fmtResourceKind(k int) string {
	return []string{"img", "buf"}[k]
}

// resource access
const (
	accessStore = iota
	accessLoad
	accessCombinedImageSampler
)

func fmtResourceAccess(a int) string {
	return []string{"store", "load", "combinedImageSampler"}[a]
}

type resource struct {
	kind   int
	id     uint64
	access int
}

func (res resource) String() string {
	return fmt.Sprintf("%s %x %s", fmtResourceKind(res.kind), res.id, fmtResourceAccess(res.access))
}

// workload kind
const (
	kindRenderpass = iota
	kindCompute
)

type workload struct {
	kind  int
	text  string
	read  map[uint64]resource
	write map[uint64]resource
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
		currentReadableImg := make(map[uint64]resource)
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

							currentRP = &workload{
								kind:  kindRenderpass,
								text:  text,
								read:  make(map[uint64]resource),
								write: make(map[uint64]resource),
							}

							for i := uint32(0); i < uint32(rpo.AttachmentDescriptions().Len()); i++ {
								if i >= uint32(fbImgs.Len()) {
									break
								}
								attachment := rpo.AttachmentDescriptions().Get(i)
								attImg := fbImgs.Get(i).Image().VulkanHandle()
								if attachment.LoadOp() == vulkan.VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD {
									log.W(ctx, "HUGUES %v consume: %v", rpID, attImg)
									currentRP.read[uint64(attImg)] = resource{
										kind:   resourceImage,
										id:     uint64(attImg),
										access: accessLoad,
									}
								}
								if attachment.StoreOp() == vulkan.VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_STORE {
									log.W(ctx, "HUGUES %v produce: %v", rpID, attImg)
									currentRP.write[uint64(attImg)] = resource{
										kind:   resourceImage,
										id:     uint64(attImg),
										access: accessStore,
									}
								}

							}

							workloads = append(workloads, currentRP)

						case vulkan.VkCmdBindDescriptorSetsArgsʳ:

							currentReadableImg = make(map[uint64]resource)
							descriptorSets := ar.DescriptorSets()

							for i := 0; i < descriptorSets.Len(); i++ {
								dsIdx := descriptorSets.Get(uint32(i))
								ds := st.DescriptorSets().Get(dsIdx)
								log.W(ctx, "HUGUES descrset: %v", ds)

								dsBindings := ds.Bindings()
								//log.W(ctx, "HUGUES bindings: %v, len: %v", dsBindings, dsBindings.Len())

								for j := 0; j < dsBindings.Len(); j++ {
									dsb := dsBindings.Get(uint32(j))
									//log.W(ctx, "HUGUES dsb: %v type: %v", dsb, dsb.BindingType())
									if dsb.BindingType() == vulkan.VkDescriptorType_VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER {
										imgBindings := dsb.ImageBinding()
										//log.W(ctx, "HUGUES imgBinding: %v len: %v", imgBindings, imgBindings.Len())
										for k := 0; k < imgBindings.Len(); k++ {
											dii := imgBindings.Get(uint32(k))
											imgViewIdx := dii.ImageView()
											//log.W(ctx, "HUGUES dii: %v imgView: %v", dii, imgViewIdx)
											imgView := st.ImageViews().Get(imgViewIdx)
											imgObj := imgView.Image()
											imgHandle := imgObj.VulkanHandle()
											//log.W(ctx, "HUGUES READ img: %v", imgHandle)
											currentReadableImg[uint64(imgHandle)] = resource{
												kind:   resourceImage,
												id:     uint64(imgHandle),
												access: accessCombinedImageSampler,
											}
										}
									}
								}
							}

						case vulkan.VkCmdDrawIndexedArgsʳ:
							q := st.LastBoundQueue().VulkanHandle()
							lastdrawinfo := st.LastDrawInfos().Get(q)
							rp := lastdrawinfo.RenderPass()
							log.W(ctx, "HUGUES drawcmdindexed, rp: %x", rp.VulkanHandle())
							for k, res := range currentReadableImg {
								log.W(ctx, "HUGUES DRAW reads: %v", k)
								currentRP.read[k] = res
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
		log.W(ctx, "HUGUES debug %+v", w)
		w.text += "\\nRead: "
		for _, res := range w.read {
			w.text += fmt.Sprintf("\\n  %v", res)
		}
		w.text += "\\nWrite:"
		for _, res := range w.write {
			w.text += fmt.Sprintf("\\n  %v", res)
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
