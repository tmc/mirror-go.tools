package ssa

// This file defines the lifting pass which tries to "lift" Alloc
// cells (new/local variables) into SSA registers, replacing loads
// with the dominating stored value, eliminating loads and stores, and
// inserting φ-nodes as needed.

// Cited papers and resources:
//
// Ron Cytron et al. 1991. Efficiently computing SSA form...
// http://doi.acm.org/10.1145/115372.115320
//
// Cooper, Harvey, Kennedy.  2001.  A Simple, Fast Dominance Algorithm.
// Software Practice and Experience 2001, 4:1-10.
// http://www.hipersoft.rice.edu/grads/publications/dom14.pdf
//
// Daniel Berlin, llvmdev mailing list, 2012.
// http://lists.cs.uiuc.edu/pipermail/llvmdev/2012-January/046638.html
// (Be sure to expand the whole thread.)

// TODO(adonovan): opt: there are many optimizations worth evaluating, and
// the conventional wisdom for SSA construction is that a simple
// algorithm well engineered often beats those of better asymptotic
// complexity on all but the most egregious inputs.
//
// Danny Berlin suggests that the Cooper et al. algorithm for
// computing the dominance frontier is superior to Cytron et al.
// Furthermore he recommends that rather than computing the DF for the
// whole function then renaming all alloc cells, it may be cheaper to
// compute the DF for each alloc cell separately and throw it away.
//
// Consider exploiting liveness information to avoid creating dead
// φ-nodes which we then immediately remove.
//
// Integrate lifting with scalar replacement of aggregates (SRA) since
// the two are synergistic.
//
// Also see many other "TODO: opt" suggestions in the code.

import (
	"fmt"
	"go/token"
	"math/big"
	"os"

	"code.google.com/p/go.tools/go/types"
)

// If true, perform sanity checking and show diagnostic information at
// each step of lifting.  Very verbose.
const debugLifting = false

// domFrontier maps each block to the set of blocks in its dominance
// frontier.  The outer slice is conceptually a map keyed by
// Block.Index.  The inner slice is conceptually a set, possibly
// containing duplicates.
//
// TODO(adonovan): opt: measure impact of dups; consider a packed bit
// representation, e.g. big.Int, and bitwise parallel operations for
// the union step in the Children loop.
//
// domFrontier's methods mutate the slice's elements but not its
// length, so their receivers needn't be pointers.
//
type domFrontier [][]*BasicBlock

func (df domFrontier) add(u, v *domNode) {
	p := &df[u.Block.Index]
	*p = append(*p, v.Block)
}

// build builds the dominance frontier df for the dominator (sub)tree
// rooted at u, using the Cytron et al. algorithm.
//
// TODO(adonovan): opt: consider Berlin approach, computing pruned SSA
// by pruning the entire IDF computation, rather than merely pruning
// the DF -> IDF step.
func (df domFrontier) build(u *domNode) {
	// Encounter each node u in postorder of dom tree.
	for _, child := range u.Children {
		df.build(child)
	}
	for _, vb := range u.Block.Succs {
		if v := vb.dom; v.Idom != u {
			df.add(u, v)
		}
	}
	for _, w := range u.Children {
		for _, vb := range df[w.Block.Index] {
			// TODO(adonovan): opt: use word-parallel bitwise union.
			if v := vb.dom; v.Idom != u {
				df.add(u, v)
			}
		}
	}
}

func buildDomFrontier(fn *Function) domFrontier {
	df := make(domFrontier, len(fn.Blocks))
	df.build(fn.Blocks[0].dom)
	return df
}

// lift attempts to replace local and new Allocs accessed only with
// load/store by SSA registers, inserting φ-nodes where necessary.
// The result is a program in classical pruned SSA form.
//
// Preconditions:
// - fn has no dead blocks (blockopt has run).
// - Def/use info (Operands and Referrers) is up-to-date.
//
func lift(fn *Function) {
	// TODO(adonovan): opt: lots of little optimizations may be
	// worthwhile here, especially if they cause us to avoid
	// buildDomTree.  For example:
	//
	// - Alloc never loaded?  Eliminate.
	// - Alloc never stored?  Replace all loads with a zero literal.
	// - Alloc stored once?  Replace loads with dominating store;
	//   don't forget that an Alloc is itself an effective store
	//   of zero.
	// - Alloc used only within a single block?
	//   Use degenerate algorithm avoiding φ-nodes.
	// - Consider synergy with scalar replacement of aggregates (SRA).
	//   e.g. *(&x.f) where x is an Alloc.
	//   Perhaps we'd get better results if we generated this as x.f
	//   i.e. Field(x, .f) instead of Load(FieldIndex(x, .f)).
	//   Unclear.
	//
	// But we will start with the simplest correct code to make
	// life easier for reviewers.

	buildDomTree(fn)

	df := buildDomFrontier(fn)

	if debugLifting {
		title := false
		for i, blocks := range df {
			if blocks != nil {
				if !title {
					fmt.Fprintln(os.Stderr, "Dominance frontier:")
					title = true
				}
				fmt.Fprintf(os.Stderr, "\t%s: %s\n", fn.Blocks[i], blocks)
			}
		}
	}

	newPhis := make(newPhiMap)

	// During this pass we will replace some BasicBlock.Instrs
	// (allocs, loads and stores) with nil, keeping a count in
	// BasicBlock.gaps.  At the end we will reset Instrs to the
	// concatenation of all non-dead newPhis and non-nil Instrs
	// for the block, reusing the original array if space permits.

	// While we're here, we also eliminate 'rundefers'
	// instructions in functions that contain no 'defer'
	// instructions.
	usesDefer := false

	// Determine which allocs we can lift and number them densely.
	// The renaming phase uses this numbering for compact maps.
	numAllocs := 0
	for _, b := range fn.Blocks {
		b.gaps = 0
		b.rundefers = 0
		for i, instr := range b.Instrs {
			switch instr := instr.(type) {
			case *Alloc:
				if liftAlloc(df, instr, newPhis) {
					instr.index = numAllocs
					numAllocs++
					// Delete the alloc.
					b.Instrs[i] = nil
					b.gaps++
				} else {
					instr.index = -1
				}
			case *Defer:
				usesDefer = true
			case *RunDefers:
				b.rundefers++
			}
		}
	}

	// renaming maps an alloc (keyed by index) to its replacement
	// value.  Initially the renaming contains nil, signifying the
	// zero literal of the appropriate type; we construct the
	// Literal lazily at most once on each path through the domtree.
	// TODO(adonovan): opt: cache per-function not per subtree.
	renaming := make([]Value, numAllocs)

	// Renaming.
	rename(fn.Blocks[0], renaming, newPhis)

	// Eliminate dead new phis, then prepend the live ones to each block.
	for _, b := range fn.Blocks {

		// Compress the newPhis slice to eliminate unused phis.
		// TODO(adonovan): opt: compute liveness to avoid
		// placing phis in blocks for which the alloc cell is
		// not live.
		nps := newPhis[b]
		j := 0
		for _, np := range nps {
			if len(*np.phi.Referrers()) == 0 {
				continue // unreferenced phi
			}
			nps[j] = np
			j++
		}
		nps = nps[:j]

		rundefersToKill := b.rundefers
		if usesDefer {
			rundefersToKill = 0
		}

		if j+b.gaps+rundefersToKill == 0 {
			continue // fast path: no new phis or gaps
		}

		// Compact nps + non-nil Instrs into a new slice.
		// TODO(adonovan): opt: compact in situ if there is
		// sufficient space or slack in the slice.
		dst := make([]Instruction, len(b.Instrs)+j-b.gaps-rundefersToKill)
		for i, np := range nps {
			dst[i] = np.phi
		}
		for _, instr := range b.Instrs {
			if instr == nil {
				continue
			}
			if !usesDefer {
				if _, ok := instr.(*RunDefers); ok {
					continue
				}
			}
			dst[j] = instr
			j++
		}
		for i, np := range nps {
			dst[i] = np.phi
		}
		b.Instrs = dst
	}

	// Remove any fn.Locals that were lifted.
	j := 0
	for _, l := range fn.Locals {
		if l.index == -1 {
			fn.Locals[j] = l
			j++
		}
	}
	// Nil out fn.Locals[j:] to aid GC.
	for i := j; i < len(fn.Locals); i++ {
		fn.Locals[i] = nil
	}
	fn.Locals = fn.Locals[:j]
}

type blockSet struct{ big.Int } // (inherit methods from Int)

// add adds b to the set and returns true if the set changed.
func (s *blockSet) add(b *BasicBlock) bool {
	i := b.Index
	if s.Bit(i) != 0 {
		return false
	}
	s.SetBit(&s.Int, i, 1)
	return true
}

// take removes an arbitrary element from a set s and
// returns its index, or returns -1 if empty.
func (s *blockSet) take() int {
	l := s.BitLen()
	for i := 0; i < l; i++ {
		if s.Bit(i) == 1 {
			s.SetBit(&s.Int, i, 0)
			return i
		}
	}
	return -1
}

// newPhi is a pair of a newly introduced φ-node and the lifted Alloc
// it replaces.
type newPhi struct {
	phi   *Phi
	alloc *Alloc
}

// newPhiMap records for each basic block, the set of newPhis that
// must be prepended to the block.
type newPhiMap map[*BasicBlock][]newPhi

// liftAlloc determines whether alloc can be lifted into registers,
// and if so, it populates newPhis with all the φ-nodes it may require
// and returns true.
//
func liftAlloc(df domFrontier, alloc *Alloc, newPhis newPhiMap) bool {
	// Don't lift aggregates into registers.
	// We'll need a separate SRA pass for that.
	switch underlyingType(indirectType(alloc.Type())).(type) {
	case *types.Array, *types.Struct:
		return false
	}

	// Compute defblocks, the set of blocks containing a
	// definition of the alloc cell.
	var defblocks blockSet
	for _, instr := range *alloc.Referrers() {
		// Bail out if we discover the alloc is not liftable;
		// the only operations permitted to use the alloc are
		// loads/stores into the cell.
		switch instr := instr.(type) {
		case *Store:
			if instr.Val == alloc {
				return false // address used as value
			}
			if instr.Addr != alloc {
				panic("Alloc.Referrers is inconsistent")
			}
			defblocks.add(instr.Block())
		case *UnOp:
			if instr.Op != token.MUL {
				return false // not a load
			}
			if instr.X != alloc {
				panic("Alloc.Referrers is inconsistent")
			}
		default:
			return false // some other instruction
		}
	}
	// The Alloc itself counts as a (zero) definition of the cell.
	defblocks.add(alloc.Block())

	if debugLifting {
		fmt.Fprintln(os.Stderr, "liftAlloc: lifting ", alloc, alloc.Name())
	}

	fn := alloc.Block().Func

	// Φ-insertion.
	//
	// What follows is the body of the main loop of the insert-φ
	// function described by Cytron et al, but instead of using
	// counter tricks, we just reset the 'hasAlready' and 'work'
	// sets each iteration.  These are bitmaps so it's pretty cheap.
	//
	// TODO(adonovan): opt: recycle slice storage for W,
	// hasAlready, defBlocks across liftAlloc calls.
	var hasAlready blockSet

	// Initialize W and work to defblocks.
	var work blockSet = defblocks // blocks seen
	var W blockSet                // blocks to do
	W.Set(&defblocks.Int)

	// Traverse iterated dominance frontier, inserting φ-nodes.
	for i := W.take(); i != -1; i = W.take() {
		u := fn.Blocks[i]
		for _, v := range df[u.Index] {
			if hasAlready.add(v) {
				// Create φ-node.
				// It will be prepended to v.Instrs later, if needed.
				phi := &Phi{
					Edges:   make([]Value, len(v.Preds)),
					Comment: alloc.Name(),
				}
				phi.setType(indirectType(alloc.Type()))
				phi.Block_ = v
				if debugLifting {
					fmt.Fprintf(os.Stderr, "place %s = %s at block %s\n", phi.Name(), phi, v)
				}
				newPhis[v] = append(newPhis[v], newPhi{phi, alloc})

				if work.add(v) {
					W.add(v)
				}
			}
		}
	}

	return true
}

// replaceAll replaces all intraprocedural uses of x with y,
// updating x.Referrers and y.Referrers.
// Precondition: x.Referrers() != nil, i.e. x must be local to some function.
//
func replaceAll(x, y Value) {
	var rands []*Value
	pxrefs := x.Referrers()
	pyrefs := y.Referrers()
	for _, instr := range *pxrefs {
		rands = instr.Operands(rands[:0]) // recycle storage
		for _, rand := range rands {
			if *rand != nil {
				if *rand == x {
					*rand = y
				}
			}
		}
		if pyrefs != nil {
			*pyrefs = append(*pyrefs, instr) // dups ok
		}
	}
	*pxrefs = nil // x is now unreferenced
}

// renamed returns the value to which alloc is being renamed,
// constructing it lazily if it's the implicit zero initialization.
//
func renamed(renaming []Value, alloc *Alloc) Value {
	v := renaming[alloc.index]
	if v == nil {
		v = zeroLiteral(indirectType(alloc.Type()))
		renaming[alloc.index] = v
	}
	return v
}

// rename implements the (Cytron et al) SSA renaming algorithm, a
// preorder traversal of the dominator tree replacing all loads of
// Alloc cells with the value stored to that cell by the dominating
// store instruction.  For lifting, we need only consider loads,
// stores and φ-nodes.
//
// renaming is a map from *Alloc (keyed by index number) to its
// dominating stored value; newPhis[x] is the set of new φ-nodes to be
// prepended to block x.
//
func rename(u *BasicBlock, renaming []Value, newPhis newPhiMap) {
	// Each φ-node becomes the new name for its associated Alloc.
	for _, np := range newPhis[u] {
		phi := np.phi
		alloc := np.alloc
		renaming[alloc.index] = phi
	}

	// Rename loads and stores of allocs.
	for i, instr := range u.Instrs {
		_ = i
		switch instr := instr.(type) {
		case *Store:
			if alloc, ok := instr.Addr.(*Alloc); ok && alloc.index != -1 { // store to Alloc cell
				// Delete the Store.
				u.Instrs[i] = nil
				u.gaps++
				// Replace dominated loads by the
				// stored value.
				renaming[alloc.index] = instr.Val
				if debugLifting {
					fmt.Fprintln(os.Stderr, "Kill store ", instr, "; current value is now ", instr.Val.Name())
				}
			}
		case *UnOp:
			if instr.Op == token.MUL {
				if alloc, ok := instr.X.(*Alloc); ok && alloc.index != -1 { // load of Alloc cell
					newval := renamed(renaming, alloc)
					if debugLifting {
						fmt.Fprintln(os.Stderr, "Replace refs to load", instr.Name(), "=", instr, "with", newval.Name())
					}
					// Replace all references to
					// the loaded value by the
					// dominating stored value.
					replaceAll(instr, newval)
					// Delete the Load.
					u.Instrs[i] = nil
					u.gaps++
				}
			}
		}
	}

	// For each φ-node in a CFG successor, rename the edge.
	for _, v := range u.Succs {
		phis := newPhis[v]
		if len(phis) == 0 {
			continue
		}
		i := v.predIndex(u)
		for _, np := range phis {
			phi := np.phi
			alloc := np.alloc
			newval := renamed(renaming, alloc)
			if debugLifting {
				fmt.Fprintf(os.Stderr, "setphi %s edge %s -> %s (#%d) (alloc=%s) := %s\n \n",
					phi.Name(), u, v, i, alloc.Name(), newval.Name())
			}
			phi.Edges[i] = newval
			if prefs := newval.Referrers(); prefs != nil {
				*prefs = append(*prefs, phi)
			}
		}
	}

	// Continue depth-first recursion over domtree, pushing a
	// fresh copy of the renaming map for each subtree.
	for _, v := range u.dom.Children {
		// TODO(adonovan): opt: avoid copy on final iteration; use destructive update.
		r := make([]Value, len(renaming))
		copy(r, renaming)
		rename(v.Block, r, newPhis)
	}
}
