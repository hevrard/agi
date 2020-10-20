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
	"github.com/google/gapid/gapis/api/sync"
	"github.com/google/gapid/gapis/api/vulkan"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/memory"
	"github.com/google/gapid/gapis/resolve"
	d2 "github.com/google/gapid/gapis/resolve/dependencygraph2"
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
func GetFramegraph2(ctx context.Context, p *path.Capture) (*service.Framegraph, error) {
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

		// there is a MutateWithSubcommands(), that takes a callback.
		// This is where we could use lastDrawInfo().se

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

	nodes := []*api.FramegraphNode{}

	for i, w := range workloads {
		for img := range w.read {
			imgCons[img] = i
		}
		for img := range w.write {
			imgProd[img] = i
		}
		nodes = append(nodes, &api.FramegraphNode{
			Id:   uint64(i),
			Type: api.FramegraphNodeType_RENDERPASS,
			Text: w.text,
		})
	}

	edges := []*api.FramegraphEdge{}
	for img, rpCons := range imgCons {
		if rpProd, ok := imgProd[img]; ok {
			edges = append(edges, &api.FramegraphEdge{
				Origin:      uint64(rpProd),
				Destination: uint64(rpCons),
			})
		}
	}

	return &service.Framegraph{Nodes: nodes, Edges: edges}, nil
}

func GetFramegraph4(ctx context.Context, p *path.Capture) (*service.Framegraph, error) {

	config := d2.DependencyGraphConfig{
		SaveNodeAccesses:       true,
		ReverseDependencies:    true,
		IncludeInitialCommands: true,
	}
	dependencyGraph, err := d2.GetDependencyGraph(ctx, p, config)
	if err != nil {
		return nil, err
	}

	hug := func(nodeID d2.NodeID, node d2.Node) error {
		switch node.(type) {

		case d2.CmdNode:
			cn := node.(d2.CmdNode)
			log.W(ctx, "HUGUES CmdNode: %v, %v", nodeID, cn.Index)
			// na := dependencyGraph.GetNodeAccesses(nodeID)
			// for _, ma := range na.MemoryAccesses {
			// 	log.W(ctx, "HUGUES %v pool: %v", ma.Mode, ma.Pool)
			// }

		case d2.ObsNode:
			log.W(ctx, "HUGUES ObsNode: %v", nodeID)
		}

		na := dependencyGraph.GetNodeAccesses(nodeID)
		// for _, ma := range na.MemoryAccesses {
		// 	log.W(ctx, "HUGUES %v pool: %v", ma.Mode, ma.Pool)
		// }
		for _, fa := range na.FragmentAccesses {
			log.W(ctx, "HUGUES FA: %v", fa)
			if field, ok := fa.Fragment.(api.Field); ok {
				log.W(ctx, "HUGUES FA Field class name: %v", field.ClassName())
			}
		}
		// for _, fw := range na.ForwardAccesses {
		// 	log.W(ctx, "HUGUES FwdA: %v", fw)
		// }

		return nil
	}
	//_ = hug
	dependencyGraph.ForeachNode(hug)

	hug2 := func(ctx context.Context, cmdID api.CmdID, cmd api.Cmd) error {
		switch cmd.(type) {

		case *vulkan.VkQueueSubmit:
			log.W(ctx, "HUGUES ### cmdID: %v", cmdID)

			nodeID := dependencyGraph.GetCmdNodeID(cmdID, []uint64{0, 0, 1})
			node := dependencyGraph.GetNode(nodeID)
			log.W(ctx, "HUGUES nodeID: %v, node: %v", nodeID, node)

			switch node.(type) {

			case d2.CmdNode:
				cn := node.(d2.CmdNode)
				log.W(ctx, "HUGUES CmdNode: %v, %v", nodeID, cn.Index)
				na := dependencyGraph.GetNodeAccesses(nodeID)
				for _, ma := range na.MemoryAccesses {
					log.W(ctx, "HUGUES %v pool: %v", ma.Mode, ma.Pool)
				}

			case d2.ObsNode:
				log.W(ctx, "HUGUES ObsNode: %v", nodeID)
			}

		}

		return nil
	}

	_ = hug2
	//dependencyGraph.ForeachCmd(ctx, hug2)

	return &service.Framegraph{}, nil
}

func GetFramegraph3(ctx context.Context, p *path.Capture) (*service.Framegraph, error) {
	config := d2.DependencyGraphConfig{
		SaveNodeAccesses:       true,
		ReverseDependencies:    true,
		IncludeInitialCommands: true,
	}
	dependencyGraph, err := d2.GetDependencyGraph(ctx, p, config)
	if err != nil {
		return nil, err
	}

	c, err := capture.ResolveGraphicsFromPath(ctx, p)
	if err != nil {
		return nil, err
	}

	postCmdCb := func(s *api.GlobalState, subCmdIdx api.SubCmdIdx, cmd api.Cmd) {
		//log.W(ctx, "HUGUES postCmdCb %v: %v", subCmdIdx, cmd)
	}
	preSubCmdCb := func(s *api.GlobalState, subCmdIdx api.SubCmdIdx, cmd api.Cmd) {
		//log.W(ctx, "HUGUES preSubCmdCb %v: %v", subCmdIdx, cmd)
	}

	postSubCmdCb := func(s *api.GlobalState, subCmdIdx api.SubCmdIdx, cmd api.Cmd) {
		log.W(ctx, "HUGUES postSubCmdCb %v: %v", subCmdIdx, cmd)
		na := dependencyGraph.GetNodeAccesses(dependencyGraph.GetCmdNodeID(api.CmdID(subCmdIdx[0]), subCmdIdx[1:]))
		mapping := pool2image(vulkan.GetState(s))
		for _, ma := range na.MemoryAccesses {
			log.W(ctx, "HUGUES %v pool: %v, images: %v", ma.Mode, ma.Pool, mapping[ma.Pool])
		}
	}
	sync.MutateWithSubcommands(ctx, p, c.Commands, postCmdCb, preSubCmdCb, postSubCmdCb)

	return &service.Framegraph{}, nil
}

func pool2image(s *vulkan.State) map[memory.PoolID]map[vulkan.VkImage]bool {
	result := make(map[memory.PoolID]map[vulkan.VkImage]bool)
	images := s.Images().All()
	for handle, image := range images {
		memInfos := image.PlaneMemoryInfo().All()
		for _, memInfo := range memInfos {
			pool := memInfo.BoundMemory().Data().Pool()
			if _, ok := result[pool]; !ok {
				result[pool] = make(map[vulkan.VkImage]bool)
			}
			result[pool][handle] = true
		}
	}
	return result
}

// results: RP1 [12 0 0 34] reads: map[pool]uint64  -- pool -> number of bytes read/written
type rpinfo struct {
	beginCmdIdx api.SubCmdIdx
	read        map[memory.PoolID]uint64
	totalRead   uint64
	write       map[memory.PoolID]uint64
	totalWrite  uint64
}

func GetFramegraph(ctx context.Context, p *path.Capture) (*service.Framegraph, error) {

	c, err := capture.ResolveGraphicsFromPath(ctx, p)
	if err != nil {
		return nil, err
	}

	// main iteration uses dependency graph
	// also use sync data to know what subcommands are

	config := d2.DependencyGraphConfig{
		SaveNodeAccesses:       true,
		ReverseDependencies:    true,
		IncludeInitialCommands: true,
	}
	dependencyGraph, err := d2.GetDependencyGraph(ctx, p, config)
	if err != nil {
		return nil, err
	}

	snc, err := resolve.SyncData(ctx, p)
	if err != nil {
		return nil, err
	}

	vulkanAPI := api.Find(vulkan.ID)
	vkSyncAPI := vulkanAPI.(sync.SynchronizedAPI)

	rpinfos := []*rpinfo{}

	process := func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		//log.W(ctx, "HUGUES cmd:%v %v", id, cmd)
		if _, ok := cmd.(*vulkan.VkQueueSubmit); ok {
			screfs, ok := snc.SubcommandReferences[id]
			if !ok {
				return log.Errf(ctx, nil, "no subcommands found for vkQueueSubmit")
			}

			var rpi *rpinfo
			insideRP := false
			for _, scref := range screfs {
				genCmdID := scref.GeneratingCmd
				var genCmd api.Cmd
				if genCmdID == api.CmdNoID {
					genCmd, err = vkSyncAPI.RecoverMidExecutionCommand(ctx, p, scref.MidExecutionCommandData)
					if err != nil {
						return err
					}
				} else {
					genCmd = c.Commands[genCmdID]
				}
				if insideRP {

					nodeID := dependencyGraph.GetCmdNodeID(id, scref.Index)
					na := dependencyGraph.GetNodeAccesses(nodeID)
					for _, ma := range na.MemoryAccesses {
						count := ma.Span.End - ma.Span.Start
						log.W(ctx, "HUGUES %v pool: %v count: %v", ma.Mode, ma.Pool, count)
						switch ma.Mode {
						case d2.ACCESS_READ:
							rpi.totalRead += count
							if _, ok := rpi.read[ma.Pool]; ok {
								rpi.read[ma.Pool] += count
							} else {
								rpi.read[ma.Pool] = count
							}
						case d2.ACCESS_WRITE:
							rpi.totalWrite += count
							if _, ok := rpi.write[ma.Pool]; ok {
								rpi.write[ma.Pool] += count
							} else {
								rpi.write[ma.Pool] = count
							}
						}
					}

					if _, ok := genCmd.(*vulkan.VkCmdEndRenderPass); ok {
						log.W(ctx, "HUGUES EndRP at %v", scref.Index)
						rpinfos = append(rpinfos, rpi)
						insideRP = false
					}
				} else {
					if _, ok := genCmd.(*vulkan.VkCmdBeginRenderPass); ok {
						log.W(ctx, "HUGUES BeginRP at %v", scref.Index)
						rpi = &rpinfo{
							beginCmdIdx: append(api.SubCmdIdx{uint64(id)}, scref.Index...),
							read:        make(map[memory.PoolID]uint64),
							write:       make(map[memory.PoolID]uint64),
						}
						insideRP = true
					}
				}
			}
		}
		return nil
	}

	err = api.ForeachCmd(ctx, c.Commands, true, process)
	if err != nil {
		return nil, err
	}

	for i, rpi := range rpinfos {
		log.W(ctx, "HUGUES rpinfo[%v]:%v", i, rpi)
	}

	// Construct framegraph
	nodes := []*api.FramegraphNode{}
	for i, rpi := range rpinfos {

		text := fmt.Sprintf("RP %v\\nRead:%v\\nWrite:%v", rpi.beginCmdIdx, rpi.totalRead, rpi.totalWrite)

		nodes = append(nodes, &api.FramegraphNode{
			Id:   uint64(i),
			Type: api.FramegraphNodeType_RENDERPASS,
			Text: text,
		})
	}

	return &service.Framegraph{Nodes: nodes}, nil
}
