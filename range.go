package merkletree

import (
	"bytes"
	"errors"
	"hash"
	"io"
	"io/ioutil"
)

// A SubtreeHasher calculates subtree roots in sequential order, for use with
// BuildRangeProof.
type SubtreeHasher interface {
	// NextSubtreeRoot returns the root of the next n leaves. If fewer than n
	// leaves are left in the tree, NextSubtreeRoot returns the root of those
	// leaves. If no leaves are left, NextSubtreeRoot returns io.EOF.
	NextSubtreeRoot(n int) ([]byte, error)
	// Skip skips the next n leaves.
	Skip(n int) error
}

// BuildRangeProof constructs a proof for the leaf range [proofStart,
// proofEnd) using the provided SubtreeHasher.
func BuildRangeProof(proofStart, proofEnd int, h SubtreeHasher) (proof [][]byte, err error) {
	if proofStart < 0 || proofStart > proofEnd || proofStart == proofEnd {
		panic("BuildRangeProof: illegal proof range")
	}

	// NOTE: this implementation is a bit magical. Essentially, the binary
	// property of Merkle trees allows us to determine which subtrees are
	// present in the proof just by looking at the binary representation of
	// the proofStart and proofEnd integers.
	//
	// As an example, imagine we are constructing the following proof:
	//
	//               ┌────────┴────────*
	//         ┌─────┴─────┐           │
	//      *──┴──┐     ┌──┴──*     ┌──┴──┐
	//    ┌─┴─┐ *─┴─┐ ┌─┴─* ┌─┴─┐ ┌─┴─┐ ┌─┴─┐
	//    0   1 2   3 4   5 6   7 8   9 10  11
	//              ^^^
	//
	// That is, proofStart = 3, proofEnd = 5, and there are 12 total leaves.
	// Each * represents a hash that should be included in the proof. But how
	// do we find these *s?
	//
	// We begin by examining the 1 bits in the binary representation of 3.
	// There are two 1 bits set, at exponents 1 and 0, which tells us that
	// there are two subtrees in the first half of the proof: one with 2^1
	// leaves, and one with 2^0 leaves. So we call NextSubtreeRoot(2) to get
	// the first proof hash, and NextSubtreeRoot(1) to get the second. The
	// order is important here: SubtreeHashers are stateful and proceed left-
	// to-right, so we should examine the bits in big-endian order to ensure
	// that we process larger subtrees first.
	//
	// The SubtreeHasher is now inside the proof range, so we call Skip to
	// proceed to the subtrees in the second half of the proof.
	//
	// We calculate the second half of the proof by examining bits again.
	// However, instead of looking at the 1 bits in proofStart, we look at the
	// 0 bits in proofEnd-1. All of the bits are 0 except for 2^2, so we call
	// NextSubtreeRoot on 2^0, 2^1, 2^3, and 2^4. Again, due to the nature of
	// the SubtreeHasher, order is important: we proceed in little-endian
	// order to ensure that we process smaller subtrees first. Finally, when
	// we attempt to call NextSubtreeRoot(2^4), it returns io.EOF, since we
	// are past the end of the tree, so the proof is complete.
	//
	// Why does this work? Well, it helps to realize that in a binary tree,
	// the bits of an leaf index describe the *path* from the root of the tree
	// to that leaf. For example: to reach leaf 2 in the above tree, start at
	// the top, then go left, left, right, left -- i.e. 0010. Thus, when we
	// want to construct a Merkle proof for a single leaf, we can use this
	// path to figure out which subtree hashes to include in the proof. But in
	// a multi-leaf proof, there are two paths: the path to proofStart, and
	// the path to proofEnd. So first we look at the path to proofStart, and
	// throw away all the hashes "to the right" of it, since we know those
	// will either be inside the proof range or part of the second half of the
	// proof; then, we look at the path to proofEnd, and do the opposite,
	// throwing away all the hashes "to the left" of it. That's why we look at
	// the 1 bits in proofStart, but the 0 bits in proofEnd. Visually:
	//
	//               ┌────────┴────────*          This is the Merkle proof for
	//         ┌─────┴─────*           │          leaf 3. Each "left-side" hash
	//      *──┴──┐     ┌──┴──┐     ┌──┴──┐       corresponds to a 1 bit in the
	//    ┌─┴─┐ *─┴─┐ ┌─┴─┐ ┌─┴─┐ ┌─┴─┐ ┌─┴─┐     binary string 1100.
	//    0   1 2   3 4   5 6   7 8   9 10  11
	//              ^
	//
	//               ┌────────┴────────*          This is the Merkle proof for
	//         *─────┴─────┐           │          leaf 4. Each "right-side" hash
	//      ┌──┴──┐     ┌──┴──*     ┌──┴──┐       corresponds to a 0 bit in the
	//    ┌─┴─┐ ┌─┴─┐ ┌─┴─* ┌─┴─┐ ┌─┴─┐ ┌─┴─┐     binary string 0010.
	//    0   1 2   3 4   5 6   7 8   9 10  11
	//                ^
	//
	// Combining the "left side" of the first proof with the "right side" of
	// the second yields the full range proof shown in the first diagram.

	// add proof hashes from leaves [0, proofStart)
	leafIndex := uint64(0)
	for subtreeSize := uint64(1 << 63); leafIndex < uint64(proofStart); subtreeSize >>= 1 {
		if uint64(proofStart)&subtreeSize != 0 {
			root, err := h.NextSubtreeRoot(int(subtreeSize))
			if err != nil {
				return nil, err
			}
			proof = append(proof, root)
			leafIndex += subtreeSize
		}
	}

	// skip leaves within proof range
	if err := h.Skip(proofEnd - proofStart); err != nil {
		// ignore EOF errors
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
	}

	// add proof hashes from proofEnd onward, stopping when NextSubtreeRoot
	// returns io.EOF.
	endMask := uint64(proofEnd - 1)
	for subtreeSize := uint64(1); subtreeSize < 1<<63; subtreeSize <<= 1 {
		if endMask&uint64(subtreeSize) == 0 {
			root, err := h.NextSubtreeRoot(int(subtreeSize))
			if err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			proof = append(proof, root)
			leafIndex += subtreeSize
		}
	}
	return proof, nil
}

// VerifyRangeProof verifies a proof produced by BuildRangeProof.
func VerifyRangeProof(leafData []byte, h hash.Hash, leafSize, proofStart, proofEnd int, proof [][]byte, root []byte) bool {
	if proofStart < 0 || proofStart > proofEnd || proofStart == proofEnd {
		panic("VerifyRangeProof: illegal proof range")
	}
	ok, err := VerifyReaderRangeProof(bytes.NewReader(leafData), h, leafSize, proofStart, proofEnd, proof, root)
	return ok && err == nil
}

// A SubtreeReader reads leaf data from an underlying stream and uses it to
// calculate subtree roots.
type SubtreeReader struct {
	r    io.Reader
	leaf []byte
	s    *Stack
}

// NextSubtreeRoot implements SubtreeHasher.
func (sr *SubtreeReader) NextSubtreeRoot(n int) ([]byte, error) {
	sr.s.Reset()
	for i := 0; i < n; i++ {
		n, err := io.ReadFull(sr.r, sr.leaf)
		if n > 0 {
			sr.s.AppendNode(sr.s.leafHash(sr.leaf[:n]))
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break // reading a partial leaf is normal at the end of the stream
		} else if err != nil {
			return nil, err
		}
	}
	if sr.s.NumNodes() == 0 {
		// we didn't read anything; return EOF
		return nil, io.EOF
	}
	return sr.s.Root(), nil
}

// Skip implements SubtreeHasher.
func (sr *SubtreeReader) Skip(n int) (err error) {
	skipSize := int64(len(sr.leaf) * n)
	if s, ok := sr.r.(io.Seeker); ok {
		_, err = s.Seek(skipSize, io.SeekCurrent)
	} else {
		// fake a seek method
		_, err = io.CopyN(ioutil.Discard, sr.r, skipSize)
	}
	return
}

// NewSubtreeReader returns a new SubtreeReader that reads leaf data from r.
func NewSubtreeReader(r io.Reader, leafSize int, h hash.Hash) *SubtreeReader {
	return &SubtreeReader{
		r:    r,
		leaf: make([]byte, leafSize),
		s:    NewStack(h),
	}
}

// BuildReaderRangeProof constructs a proof for the range [proofStart,
// proofEnd), using leaf data read from r.
func BuildReaderRangeProof(r io.Reader, h hash.Hash, leafSize, proofStart, proofEnd int) ([][]byte, error) {
	if proofStart < 0 || proofStart > proofEnd || proofStart == proofEnd {
		panic("BuildReaderRangeProof: illegal proof range")
	}
	return BuildRangeProof(proofStart, proofEnd, NewSubtreeReader(r, leafSize, h))
}

// VerifyReaderRangeProof verifies a proof produced by BuildRangeProof, using
// leaf data read from r, which must contain only the leaves within the proof
// range.
func VerifyReaderRangeProof(r io.Reader, h hash.Hash, leafSize, proofStart, proofEnd int, proof [][]byte, root []byte) (bool, error) {
	if proofStart < 0 || proofStart > proofEnd || proofStart == proofEnd {
		panic("VerifyReaderRangeProof: illegal proof range")
	}

	// manually build a stack using the proof hashes
	s := NewStack(h)

	// add proof hashes up to proofStart
	for i := uint64(63); i != ^uint64(0) && len(proof) > 0; i-- {
		subtreeSize := 1 << i
		if proofStart&subtreeSize != 0 {
			s.appendNodeAtHeight(proof[0], i)
			proof = proof[1:]
		}
	}

	// add leaf hashes
	leaf := make([]byte, leafSize)
	for i := proofStart; i < proofEnd; i++ {
		n, err := io.ReadFull(r, leaf)
		if n > 0 {
			s.AppendNode(s.leafHash(leaf[:n]))
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if i == proofEnd-1 {
				break // last leaf was partial
			}
			return false, errors.New("insufficient leaf data in reader")
		} else if err != nil {
			return false, err
		}
	}

	// add proof hashes after proofEnd
	endMask := 0 - uint64(proofEnd)
	for i := uint64(0); len(proof) > 0; i++ {
		subtreeSize := 1 << i
		if endMask&uint64(subtreeSize) != 0 {
			s.appendNodeAtHeight(proof[0], i)
			proof = proof[1:]
		}
	}

	return bytes.Equal(s.Root(), root), nil
}
