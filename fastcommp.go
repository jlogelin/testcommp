package fastcommp

import (
	"math/bits"
	"runtime"

	"github.com/filecoin-project/go-commp-utils/nonffi"
	commcid "github.com/filecoin-project/go-fil-commcid"
	commp "github.com/filecoin-project/go-fil-commp-hashhash"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/go-commp-utils/zerocomm"
)

// DataCIDSize is the result of a DataCID calculation
type DataCIDSize struct {
	PayloadSize int64
	PieceSize   abi.PaddedPieceSize
	PieceCID    cid.Cid
}

// commPBufPad is the size of the buffer used to calculate commP
const commPBufPad = abi.PaddedPieceSize(8 << 20)

// CommPBuf is the size of the buffer used to calculate commP
const CommPBuf = abi.UnpaddedPieceSize(commPBufPad - (commPBufPad / 128))

// ciderr is a cid and an error
type ciderr struct {
	c   cid.Cid
	err error
}

// CommpWriter is a writer that calculates the CommP
type CommpWriter struct {
	len    int64
	buf    [CommPBuf]byte
	leaves []chan ciderr

	tbufs    [][CommPBuf]byte
	throttle chan int
}

// Write writes data to the DataCidWriter
func (w *CommpWriter) Write(p []byte) (int, error) {
	if w.throttle == nil {
		w.throttle = make(chan int, runtime.NumCPU())
	}
	for i := 0; i < cap(w.throttle); i++ {
		w.throttle <- i
	}
	if w.tbufs == nil {
		w.tbufs = make([][CommPBuf]byte, cap(w.throttle))
	}

	// process last non-zero leaf if exists and we have data to write
	n := len(p)
	for len(p) > 0 {
		buffered := int(w.len % int64(len(w.buf)))
		toBuffer := len(w.buf) - buffered
		if toBuffer > len(p) {
			toBuffer = len(p)
		}

		copied := copy(w.buf[buffered:], p[:toBuffer])
		p = p[copied:]
		w.len += int64(copied)

		// if we filled the buffer, process it
		if copied > 0 && w.len%int64(len(w.buf)) == 0 {
			leaf := make(chan ciderr, 1)
			bufIdx := <-w.throttle
			copy(w.tbufs[bufIdx][:], w.buf[:])

			// process leaf in a goroutine
			go func() {
				defer func() {
					w.throttle <- bufIdx
				}()

				// calculate commP for this leaf and send it to the channel
				cc := new(commp.Calc)
				_, _ = cc.Write(w.tbufs[bufIdx][:])
				p, _, _ := cc.Digest()
				l, _ := commcid.PieceCommitmentV1ToCID(p)
				leaf <- ciderr{
					c:   l,
					err: nil,
				}
			}()

			// add leaf to list
			w.leaves = append(w.leaves, leaf)
		}
	}
	return n, nil
}

func (w *CommpWriter) Sum() (DataCIDSize, error) {
	// process last non-zero leaf if exists
	lastLen := w.len % int64(len(w.buf))
	rawLen := w.len

	// wait for all leaves to finish
	leaves := make([]cid.Cid, len(w.leaves))
	for i, leaf := range w.leaves {
		r := <-leaf
		if r.err != nil {
			return DataCIDSize{}, xerrors.Errorf("processing leaf %d: %w", i, r.err)
		}
		leaves[i] = r.c
	}

	// process remaining bit of data
	if lastLen != 0 {
		if len(leaves) != 0 {
			copy(w.buf[lastLen:], make([]byte, int(int64(CommPBuf)-lastLen)))
			lastLen = int64(CommPBuf)
		}

		cc := new(commp.Calc)
		_, _ = cc.Write(w.buf[:lastLen])
		pb, pps, _ := cc.Digest()
		p, _ := commcid.PieceCommitmentV1ToCID(pb)

		// if the last piece is less than CommPBuf, we're done
		if abi.PaddedPieceSize(pps).Unpadded() < CommPBuf {
			return DataCIDSize{
				PayloadSize: w.len,
				PieceSize:   abi.PaddedPieceSize(pps),
				PieceCID:    p,
			}, nil
		}

		leaves = append(leaves, p)
	}

	// pad with zero pieces to power-of-two size
	fillerLeaves := (1 << (bits.Len(uint(len(leaves) - 1)))) - len(leaves)
	for i := 0; i < fillerLeaves; i++ {
		leaves = append(leaves, zerocomm.ZeroPieceCommitment(CommPBuf))
	}

	if len(leaves) == 1 {
		return DataCIDSize{
			PayloadSize: rawLen,
			PieceSize:   abi.PaddedPieceSize(len(leaves)) * commPBufPad,
			PieceCID:    leaves[0],
		}, nil
	}

	pieces := make([]abi.PieceInfo, len(leaves))
	for i, leaf := range leaves {
		pieces[i] = abi.PieceInfo{
			Size:     commPBufPad,
			PieceCID: leaf,
		}
	}

	p, err := nonffi.GenerateUnsealedCID(abi.RegisteredSealProof_StackedDrg32GiBV1, pieces)
	if err != nil {
		return DataCIDSize{}, xerrors.Errorf("generating unsealed CID: %w", err)
	}

	return DataCIDSize{
		PayloadSize: rawLen,
		PieceSize:   abi.PaddedPieceSize(len(leaves)) * commPBufPad,
		PieceCID:    p,
	}, nil
}
