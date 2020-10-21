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
	"github.com/google/gapid/core/math/interval"
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

type stateResourceMapping struct {
	images  map[vulkan.VkImage]map[memory.PoolID][]interval.U64Span
	buffers map[vulkan.VkBuffer]map[memory.PoolID][]interval.U64Span
	//devMem  map[vulkan.VkDeviceMemory]map[memory.PoolID][]interval.U64Span
}

func createStateResourceMapping(s *vulkan.State) stateResourceMapping {
	srm := stateResourceMapping{
		images:  make(map[vulkan.VkImage]map[memory.PoolID][]interval.U64Span),
		buffers: make(map[vulkan.VkBuffer]map[memory.PoolID][]interval.U64Span),
		//devMem:  make(map[vulkan.VkDeviceMemory]map[memory.PoolID][]interval.U64Span),
	}

	// check we do access all layers level data??
	// debugger in dovkcmdendrenderpass accessimagesubresource
	images := s.Images().All()
	for handle, image := range images {
		fmt.Printf("HUGUES SRM img: %v\n", handle)
		if _, ok := srm.images[handle]; !ok {
			srm.images[handle] = make(map[memory.PoolID][]interval.U64Span)
		}
		for _, aspect := range image.Aspects().All() {
			for _, layer := range aspect.Layers().All() {
				for _, level := range layer.Levels().All() {
					data := level.Data()
					pool := data.Pool()
					if _, ok := srm.images[handle][pool]; !ok {
						srm.images[handle][pool] = []interval.U64Span{}
					}
					fmt.Printf("HUGUES SRM img: %v pool:%v\n", handle, pool)
					srm.images[handle][pool] = append(srm.images[handle][pool], data.Range().Span())
				}
			}
		}
		planeMemInfos := image.PlaneMemoryInfo().All()
		for _, memInfo := range planeMemInfos {
			data := memInfo.BoundMemory().Data()
			pool := data.Pool()
			if _, ok := srm.images[handle][pool]; !ok {
				srm.images[handle][pool] = []interval.U64Span{}
			}
			//fmt.Printf("HUGUES SRM img: %v pool:%v base:%v\n", handle, pool, data.Base())
			srm.images[handle][pool] = append(srm.images[handle][pool], data.Range().Span())
		}
	}

	buffers := s.Buffers().All()
	for handle, buffer := range buffers {
		if _, ok := srm.buffers[handle]; !ok {
			srm.buffers[handle] = make(map[memory.PoolID][]interval.U64Span)
		}
		data := buffer.Memory().Data()
		pool := data.Pool()
		if _, ok := srm.buffers[handle][pool]; !ok {
			srm.buffers[handle][pool] = []interval.U64Span{}
		}
		srm.buffers[handle][pool] = append(srm.buffers[handle][pool], data.Range().Span())
	}

	devMems := s.DeviceMemories().All()
	for _, devMem := range devMems {
		data := devMem.Data()
		pool := data.Pool()
		span := data.Range().Span()
		boundObj := devMem.BoundObjects().All()
		// TODO: deal with memory offset from boundObj
		found := false
		for objHandle := range boundObj {
			imgHandle := vulkan.VkImage(objHandle)
			if _, ok := srm.images[imgHandle]; ok {
				if found {
					fmt.Printf("\nHUGUES double handle: %v", imgHandle)
				}
				found = true
				if _, ok := srm.images[imgHandle][pool]; !ok {
					srm.images[imgHandle][pool] = []interval.U64Span{}
				}
				srm.images[imgHandle][pool] = append(srm.images[imgHandle][pool], span)
			}
			bufHandle := vulkan.VkBuffer(objHandle)
			if _, ok := srm.buffers[bufHandle]; ok {
				if found {
					fmt.Printf("\nHUGUES double handle: %v", bufHandle)
				}
				found = true
				if _, ok := srm.buffers[bufHandle][pool]; !ok {
					srm.buffers[bufHandle][pool] = []interval.U64Span{}
				}
				srm.buffers[bufHandle][pool] = append(srm.buffers[bufHandle][pool], span)
			}
			if !found {
				fmt.Printf("\nHUGUES objHandle NOT FOUND: %v", objHandle)
			}
		}
	}

	return srm
}

func (s stateResourceMapping) resourceLookup(poolID memory.PoolID, span interval.U64Span) (resource, bool) {
	for img, mem := range s.images {
		if intervals, ok := mem[poolID]; ok {
			//fmt.Printf("\nHUGUES resLookup IMG:%v poolID:%v\n", img, poolID)
			for _, interval := range intervals {
				if interval.Start <= span.Start && span.End <= interval.End {
					return resource{
						kind: resourceImage,
						id:   uint64(img),
					}, true
				}
			}
		}
	}

	for buf, mem := range s.buffers {
		if intervals, ok := mem[poolID]; ok {
			//fmt.Printf("\nHUGUES resLookup BUF:%v poolID:%v\n", buf, poolID)
			for _, interval := range intervals {
				if interval.Start <= span.Start && span.End <= interval.End {
					return resource{
						kind: resourceBuffer,
						id:   uint64(buf),
					}, true
				}
			}
		}
	}

	//fmt.Printf("\nHUGUES resLookup FAIL poolID:%v span:%v\n", poolID, span)
	return resource{}, false
}

// results: RP1 [12 0 0 34] reads: map[pool]uint64  -- pool -> number of bytes read/written
type rpinfo struct {
	beginCmdIdx api.SubCmdIdx
	dpNodes     map[d2.NodeID]bool
	read        map[memory.PoolID]uint64
	totalRead   uint64
	write       map[memory.PoolID]uint64
	totalWrite  uint64
	imgRead     map[uint64]uint64
	imgWrite    map[uint64]uint64
	bufRead     map[uint64]uint64
	bufWrite    map[uint64]uint64
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

	state := c.NewState(ctx)

	vulkanAPI := api.Find(vulkan.ID)
	vkSyncAPI := vulkanAPI.(sync.SynchronizedAPI)

	rpinfos := []*rpinfo{}

	process := func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {

		// Start by mutating the command
		err := cmd.Mutate(ctx, id, state, nil, nil)
		if err != nil {
			return err
		}

		// Process queueSubmits
		if _, ok := cmd.(*vulkan.VkQueueSubmit); ok {
			screfs, ok := snc.SubcommandReferences[id]
			if !ok {
				return log.Errf(ctx, nil, "no subcommands found for vkQueueSubmit")
			}

			srm := createStateResourceMapping(vulkan.GetState(state))
			log.W(ctx, "HUGUES srm: %+v", srm)

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

					log.W(ctx, "HUGUES insideRP cmd:%v", genCmd)

					nodeID := dependencyGraph.GetCmdNodeID(id, scref.Index)
					rpi.dpNodes[nodeID] = true
					na := dependencyGraph.GetNodeAccesses(nodeID)
					for _, ma := range na.MemoryAccesses {
						count := ma.Span.End - ma.Span.Start
						log.W(ctx, "HUGUES %v pool: %v span: %v", ma.Mode, ma.Pool, ma.Span)
						switch ma.Mode {
						case d2.ACCESS_READ:
							rpi.totalRead += count
							if _, ok := rpi.read[ma.Pool]; ok {
								rpi.read[ma.Pool] += count
							} else {
								rpi.read[ma.Pool] = count
							}
							if res, ok := srm.resourceLookup(ma.Pool, ma.Span); ok {
								//log.W(ctx, "HUGUES hit read resource: %v", res)
								switch res.kind {
								case resourceImage:
									if _, ok := rpi.imgRead[res.id]; ok {
										rpi.imgRead[res.id] += count
									} else {
										rpi.imgRead[res.id] = count
									}
								case resourceBuffer:
									if _, ok := rpi.bufRead[res.id]; ok {
										rpi.bufRead[res.id] += count
									} else {
										rpi.bufRead[res.id] = count
									}
								}
							} else {
								log.W(ctx, "HUGUES resLookup FAIL pool:%v span:%v\n", ma.Pool, ma.Span)
							}
						case d2.ACCESS_WRITE:
							rpi.totalWrite += count
							if _, ok := rpi.write[ma.Pool]; ok {
								rpi.write[ma.Pool] += count
							} else {
								rpi.write[ma.Pool] = count
							}

							if res, ok := srm.resourceLookup(ma.Pool, ma.Span); ok {
								//log.W(ctx, "HUGUES hit write resource: %v", res)
								switch res.kind {
								case resourceImage:
									if _, ok := rpi.imgWrite[res.id]; ok {
										rpi.imgWrite[res.id] += count
									} else {
										rpi.imgWrite[res.id] = count
									}
								case resourceBuffer:
									if _, ok := rpi.bufWrite[res.id]; ok {
										rpi.bufWrite[res.id] += count
									} else {
										rpi.bufWrite[res.id] = count
									}
								}
							} else {
								log.W(ctx, "HUGUES resLookup FAIL pool:%v span:%v", ma.Pool, ma.Span)
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
						nodeID := dependencyGraph.GetCmdNodeID(id, scref.Index)
						rpi = &rpinfo{
							beginCmdIdx: append(api.SubCmdIdx{uint64(id)}, scref.Index...),
							read:        make(map[memory.PoolID]uint64),
							write:       make(map[memory.PoolID]uint64),
							dpNodes:     map[d2.NodeID]bool{nodeID: true},
							imgRead:     make(map[uint64]uint64),
							imgWrite:    make(map[uint64]uint64),
							bufRead:     make(map[uint64]uint64),
							bufWrite:    make(map[uint64]uint64),
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

		// Use "\l" for newlines as this produce left-align lines in graphviz DOT labels
		text := fmt.Sprintf("RP %v\\lRead:%v bytes\\lWrite:%v bytes\\l", rpi.beginCmdIdx, rpi.totalRead, rpi.totalWrite)
		for img, bytes := range rpi.imgRead {
			text += fmt.Sprintf("%x img read(%v)\\l", img, bytes)
		}
		for img, bytes := range rpi.imgWrite {
			text += fmt.Sprintf("%x img write(%v)\\l", img, bytes)
		}
		for buf, bytes := range rpi.bufRead {
			text += fmt.Sprintf("%x buf read(%v)\\l", buf, bytes)
		}
		for buf, bytes := range rpi.bufWrite {
			text += fmt.Sprintf("%x buf write(%v)\\l", buf, bytes)
		}

		nodes = append(nodes, &api.FramegraphNode{
			Id:   uint64(i),
			Type: api.FramegraphNodeType_RENDERPASS,
			Text: text,
		})
	}

	edges := []*api.FramegraphEdge{}
	for i, srcRpi := range rpinfos {
		dependsOn := map[int]bool{}
		for src := range srcRpi.dpNodes {
			dependencyGraph.ForeachDependencyFrom(src, func(nodeID d2.NodeID) error {
				for j, dstRpi := range rpinfos {
					if dstRpi == srcRpi {
						continue
					}
					if _, ok := dstRpi.dpNodes[nodeID]; ok {
						dependsOn[j] = true
					}
				}
				return nil
			})
		}
		for dep := range dependsOn {
			edges = append(edges, &api.FramegraphEdge{
				Origin:      uint64(i),
				Destination: uint64(dep),
			})
		}
	}

	return &service.Framegraph{Nodes: nodes, Edges: edges}, nil
}
