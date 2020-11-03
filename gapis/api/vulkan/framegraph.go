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

package vulkan

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/api/sync"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/memory"
	"github.com/google/gapid/gapis/resolve/dependencygraph2"
	"github.com/google/gapid/gapis/service/path"
)

type imageInfo struct {
	handle  uint64
	format  VkFormat
	imgType VkImageType
	width   uint32
	height  uint32
	depth   uint32
}

type bufferInfo struct {
	handle uint64
	size   uint64
}

// renderpassInfo stores a renderpass' info relevant for the framegraph.
type renderpassInfo struct {
	id       uint64
	beginIdx api.SubCmdIdx
	endIdx   api.SubCmdIdx
	nodes    []dependencygraph2.NodeID
	deps     map[uint64]struct{} // set of renderpasses this renderpass depends on
	readImg  map[uint64]*imageInfo
	writeImg map[uint64]*imageInfo
	readBuf  map[uint64]*bufferInfo
	writeBuf map[uint64]*bufferInfo
}

// nodeLabel formats a renderpassInfo into a string usable as a Graphviz DOT node label.
func (rpInfo *renderpassInfo) nodeLabel() string {
	// Graphviz DOT: use "\l" as a newline to obtain left-aligned text.
	// List resources sorted by their handle values.

	read := "Read:\\l"
	handles := make([]uint64, 0, len(rpInfo.readImg))
	for h := range rpInfo.readImg {
		handles = append(handles, h)
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
	for _, h := range handles {
		read += rpInfo.readImg[h].nodeLabel()
	}
	handles = make([]uint64, 0, len(rpInfo.readBuf))
	for h := range rpInfo.readBuf {
		handles = append(handles, h)
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
	for _, h := range handles {
		read += rpInfo.readBuf[h].nodeLabel()
	}

	write := "Write:\\l"
	handles = make([]uint64, 0, len(rpInfo.writeImg))
	for h := range rpInfo.writeImg {
		handles = append(handles, h)
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
	for _, h := range handles {
		write += rpInfo.writeImg[h].nodeLabel()
	}
	handles = make([]uint64, 0, len(rpInfo.writeBuf))
	for h := range rpInfo.writeBuf {
		handles = append(handles, h)
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
	for _, h := range handles {
		write += rpInfo.writeBuf[h].nodeLabel()
	}

	// Space after the "end:" is to align the subcommand indexes:
	//   start:[1 0 0 2]
	//   end:  [1 0 0 4]
	return fmt.Sprintf("Renderpass %v\\lbegin:%v\\lend:  %v\\l\\l%s\\l%s\\l", rpInfo.id, rpInfo.beginIdx, rpInfo.endIdx, read, write)
}

// nodeLabel formats an imageInfo into a string usable as a Graphviz DOT node label.
func (imgInfo *imageInfo) nodeLabel() string {
	imgType := strings.TrimPrefix(fmt.Sprintf("%v", imgInfo.imgType), "VK_IMAGE_TYPE_")
	format := strings.TrimPrefix(fmt.Sprintf("%v", imgInfo.format), "VK_FORMAT_")
	return fmt.Sprintf("Img 0x%X [%vx%vx%v] %v %v\\l", imgInfo.handle, imgInfo.width, imgInfo.height, imgInfo.depth, imgType, format)
}

// nodeLabel formats a bufferInfo into a string usable as a Graphviz DOT node label.
func (bufInfo *bufferInfo) nodeLabel() string {
	return fmt.Sprintf("Buf 0x%X [%v]\\l", bufInfo.handle, bufInfo.size)
}

// createImageLookup creates a lookup table to quickly find an image matching a memory observation.
func createImageLookup(state *State) map[memory.PoolID]map[memory.Range][]*ImageObjectʳ {
	imageLookup := make(map[memory.PoolID]map[memory.Range][]*ImageObjectʳ)
	for _, image := range state.Images().All() {
		for _, aspect := range image.Aspects().All() {
			for _, layer := range aspect.Layers().All() {
				for _, level := range layer.Levels().All() {
					pool := level.Data().Pool()
					memRange := level.Data().Range()
					if _, ok := imageLookup[pool]; !ok {
						imageLookup[pool] = make(map[memory.Range][]*ImageObjectʳ)
					}
					imageLookup[pool][memRange] = append(imageLookup[pool][memRange], &image)
				}
			}
		}
	}
	return imageLookup
}

// lookupImages returns all the images that contain (pool, memRange).
func lookupImages(imageLookup map[memory.PoolID]map[memory.Range][]*ImageObjectʳ, pool memory.PoolID, memRange memory.Range) []*ImageObjectʳ {
	images := []*ImageObjectʳ{}
	for imgRange := range imageLookup[pool] {
		if imgRange.Includes(memRange) {
			images = append(images, imageLookup[pool][imgRange]...)
		}
	}
	return images
}

// createBufferLookup creates a lookup table to quickly find a buffer matching a memory observation.
func createBufferLookup(state *State) map[memory.PoolID]map[memory.Range][]*BufferObjectʳ {
	bufferLookup := make(map[memory.PoolID]map[memory.Range][]*BufferObjectʳ)
	for _, buffer := range state.Buffers().All() {
		pool := buffer.Memory().Data().Pool()
		memRange := memory.Range{
			Base: uint64(buffer.MemoryOffset()),
			Size: uint64(buffer.Info().Size()),
		}
		if _, ok := bufferLookup[pool]; !ok {
			bufferLookup[pool] = make(map[memory.Range][]*BufferObjectʳ)
		}
		bufferLookup[pool][memRange] = append(bufferLookup[pool][memRange], &buffer)
	}
	return bufferLookup
}

// lookupBuffers returns all the buffers that contain (pool, memRange).
func lookupBuffers(bufferLookup map[memory.PoolID]map[memory.Range][]*BufferObjectʳ, pool memory.PoolID, memRange memory.Range) []*BufferObjectʳ {
	buffers := []*BufferObjectʳ{}
	for bufRange := range bufferLookup[pool] {
		if bufRange.Includes(memRange) {
			buffers = append(buffers, bufferLookup[pool][bufRange]...)
		}
	}
	return buffers
}

// framegraphInfoHelpers contains variables that stores information while
// processing subCommands.
type framegraphInfoHelpers struct {
	rpInfos      []*renderpassInfo
	rpInfo       *renderpassInfo
	currRpId     uint64
	imageLookup  map[memory.PoolID]map[memory.Range][]*ImageObjectʳ
	bufferLookup map[memory.PoolID]map[memory.Range][]*BufferObjectʳ
}

// processSubCommand records framegraph information upon each subcommand.
func processSubCommand(ctx context.Context, helpers *framegraphInfoHelpers, dependencyGraph dependencygraph2.DependencyGraph, state *api.GlobalState, subCmdIdx api.SubCmdIdx, cmd api.Cmd, i interface{}) {
	vkState := GetState(state)
	cmdRef, ok := i.(CommandReferenceʳ)
	if !ok {
		panic("In Vulkan, MutateWithSubCommands' postSubCmdCb 'interface{}' is not a CommandReferenceʳ")
	}
	cmdArgs := GetCommandArgs(ctx, cmdRef, vkState)

	// Beginning of renderpass
	if _, ok := cmdArgs.(VkCmdBeginRenderPassArgsʳ); ok {
		if helpers.rpInfo != nil {
			panic("Renderpass starts without having ended")
		}
		helpers.rpInfo = &renderpassInfo{
			id:       helpers.currRpId,
			beginIdx: subCmdIdx,
			nodes:    []dependencygraph2.NodeID{},
			deps:     make(map[uint64]struct{}),
			readImg:  make(map[uint64]*imageInfo),
			writeImg: make(map[uint64]*imageInfo),
			readBuf:  make(map[uint64]*bufferInfo),
			writeBuf: make(map[uint64]*bufferInfo),
		}
		helpers.currRpId++
		// Create temporary lookup tables for buffers and images. This leads to
		// significant speedups on some big games, saving several seconds of
		// computation.
		// TODO(hevrard): lookup table could be created only once per vkQueueSubmit
		helpers.imageLookup = createImageLookup(vkState)
		helpers.bufferLookup = createBufferLookup(vkState)
	}

	// Process commands that are inside a renderpass
	if helpers.rpInfo != nil {
		nodeID := dependencyGraph.GetCmdNodeID(api.CmdID(subCmdIdx[0]), subCmdIdx[1:])
		helpers.rpInfo.nodes = append(helpers.rpInfo.nodes, nodeID)

		// Analyze memory accesses
		for _, memAccess := range dependencyGraph.GetNodeAccesses(nodeID).MemoryAccesses {
			memRange := memory.Range{
				Base: memAccess.Span.Start,
				Size: memAccess.Span.End - memAccess.Span.Start,
			}
			images := lookupImages(helpers.imageLookup, memAccess.Pool, memRange)
			for _, image := range images {
				handle := uint64(image.VulkanHandle())
				imgInfo := &imageInfo{
					handle:  handle,
					format:  image.Info().Fmt(),
					imgType: image.Info().ImageType(),
					width:   image.Info().Extent().Width(),
					height:  image.Info().Extent().Height(),
					depth:   image.Info().Extent().Depth(),
				}
				switch memAccess.Mode {
				case dependencygraph2.ACCESS_READ:
					helpers.rpInfo.readImg[handle] = imgInfo
				case dependencygraph2.ACCESS_WRITE:
					helpers.rpInfo.writeImg[handle] = imgInfo
				}
			}
			for _, buffer := range lookupBuffers(helpers.bufferLookup, memAccess.Pool, memRange) {
				handle := uint64(buffer.VulkanHandle())
				bufInfo := &bufferInfo{
					handle: handle,
					size:   uint64(buffer.Info().Size()),
				}
				switch memAccess.Mode {
				case dependencygraph2.ACCESS_READ:
					helpers.rpInfo.readBuf[handle] = bufInfo
				case dependencygraph2.ACCESS_WRITE:
					helpers.rpInfo.writeBuf[handle] = bufInfo
				}
			}
		}
	}

	// Ending of renderpass
	if _, ok := cmdArgs.(VkCmdEndRenderPassArgsʳ); ok {
		if helpers.rpInfo == nil {
			panic("Renderpass ends without having started")
		}
		helpers.rpInfo.endIdx = subCmdIdx
		helpers.rpInfos = append(helpers.rpInfos, helpers.rpInfo)
		helpers.rpInfo = nil
	}
}

// GetFramegraph creates the framegraph of the given capture.
func (API) GetFramegraph(ctx context.Context, p *path.Capture) (*api.Framegraph, error) {
	config := dependencygraph2.DependencyGraphConfig{
		SaveNodeAccesses:    true,
		ReverseDependencies: true,
	}
	dependencyGraph, err := dependencygraph2.GetDependencyGraph(ctx, p, config)
	if err != nil {
		return nil, err
	}

	// postSubCmdCb effectively processes each subcommand to extract renderpass
	// info, while recording information into the helpers.
	helpers := &framegraphInfoHelpers{
		rpInfos:  []*renderpassInfo{},
		rpInfo:   nil,
		currRpId: uint64(0),
	}
	postSubCmdCb := func(state *api.GlobalState, subCmdIdx api.SubCmdIdx, cmd api.Cmd, i interface{}) {
		processSubCommand(ctx, helpers, dependencyGraph, state, subCmdIdx, cmd, i)
	}

	// Iterate on the capture commands to collect information
	c, err := capture.ResolveGraphicsFromPath(ctx, p)
	if err != nil {
		return nil, err
	}
	if err := sync.MutateWithSubcommands(ctx, p, c.Commands, nil, nil, postSubCmdCb); err != nil {
		return nil, err
	}

	updateDependencies(helpers.rpInfos, dependencyGraph)

	// Build the framegraph nodes and edges from collected data.
	nodes := make([]*api.FramegraphNode, len(helpers.rpInfos))
	for i, rpInfo := range helpers.rpInfos {
		// Graphviz DOT: use "\l" as a newline to obtain left-aligned text.
		nodes[i] = &api.FramegraphNode{
			Id:   rpInfo.id,
			Text: rpInfo.nodeLabel(),
		}
	}

	edges := []*api.FramegraphEdge{}
	for _, rpInfo := range helpers.rpInfos {
		for deps := range rpInfo.deps {
			edges = append(edges, &api.FramegraphEdge{
				// We want the graph to show the flow of how the frame is
				// created (rather than the flow of dependencies), so use the
				// dependency as the edge origin and rpInfo as the destination.
				Origin:      deps,
				Destination: rpInfo.id,
			})
		}
	}

	return &api.Framegraph{Nodes: nodes, Edges: edges}, nil
}

// updateDependencies establishes dependencies between renderpasses.
func updateDependencies(rpInfos []*renderpassInfo, dependencyGraph dependencygraph2.DependencyGraph) {
	// isInsideRenderpass: node -> renderpass it belongs to.
	isInsideRenderpass := map[dependencygraph2.NodeID]uint64{}
	for _, rpInfo := range rpInfos {
		for _, n := range rpInfo.nodes {
			isInsideRenderpass[n] = rpInfo.id
		}
	}
	// node2renderpasses: node -> set of renderpasses it depends on.
	node2renderpasses := map[dependencygraph2.NodeID]map[uint64]struct{}{}

	// For a given renderpass RP, for each of its node, explore the dependency
	// graph in reverse order to mark all the nodes dependending on RP until we
	// hit the node of another renderpass, which then depends on RP.
	for _, rpInfo := range rpInfos {
		// markNode is recursive, so declare it before initializing it.
		var markNode func(dependencygraph2.NodeID) error
		markNode = func(node dependencygraph2.NodeID) error {
			if id, ok := isInsideRenderpass[node]; ok {
				if id != rpInfo.id {
					// Reached a node that is inside another renderpass, so this
					// renderpass depends on rpInfo.
					rpInfos[id].deps[rpInfo.id] = struct{}{}
				}
				return nil
			}
			if _, ok := node2renderpasses[node]; !ok {
				node2renderpasses[node] = map[uint64]struct{}{}
			}
			if _, ok := node2renderpasses[node][rpInfo.id]; ok {
				// Node already visited, stop recursion
				return nil
			}
			node2renderpasses[node][rpInfo.id] = struct{}{}
			return dependencyGraph.ForeachDependencyTo(node, markNode)
		}
		for _, node := range rpInfo.nodes {
			dependencyGraph.ForeachDependencyTo(node, markNode)
		}
	}
}
