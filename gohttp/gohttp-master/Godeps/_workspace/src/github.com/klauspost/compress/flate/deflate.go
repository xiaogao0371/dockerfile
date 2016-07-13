// Copyright 2009 The Go Authors. All rights reserved.
// Copyright (c) 2015 Klaus Post
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

import (
	"fmt"
	"io"
	"math"
)

const (
	NoCompression       = 0
	BestSpeed           = 1
	fastCompression     = 3
	BestCompression     = 9
	DefaultCompression  = -1
	ConstantCompression = -2 // Does only Huffman encoding
	logWindowSize       = 15
	windowSize          = 1 << logWindowSize
	windowMask          = windowSize - 1
	logMaxOffsetSize    = 15  // Standard DEFLATE
	minMatchLength      = 4   // The smallest match that the compressor looks for
	maxMatchLength      = 258 // The longest match for the compressor
	minOffsetSize       = 1   // The shortest offset that makes any sense

	// The maximum number of tokens we put into a single flat block, just too
	// stop things from getting too large.
	maxFlateBlockTokens = 1 << 14
	maxStoreBlockSize   = 65535
	hashBits            = 17 // After 17 performance degrades
	hashSize            = 1 << hashBits
	hashMask            = (1 << hashBits) - 1
	hashShift           = (hashBits + minMatchLength - 1) / minMatchLength
	maxHashOffset       = 1 << 24

	skipNever = math.MaxInt32
)

var useSSE42 bool

type compressionLevel struct {
	good, lazy, nice, chain, fastSkipHashing int
}

var levels = []compressionLevel{
	{}, // 0
	// For levels 1-3 we don't bother trying with lazy matches
	{4, 0, 8, 4, 4},
	{4, 0, 16, 8, 5},
	{4, 0, 32, 32, 6},
	// Levels 4-9 use increasingly more lazy matching
	// and increasingly stringent conditions for "good enough".
	{4, 4, 16, 16, skipNever},
	{8, 16, 32, 32, skipNever},
	{8, 16, 128, 128, skipNever},
	{8, 32, 128, 256, skipNever},
	{32, 128, 258, 1024, skipNever},
	{32, 258, 258, 4096, skipNever},
}

type hashid uint32

type compressor struct {
	compressionLevel

	w          *huffmanBitWriter
	hasher     func([]byte) hash
	bulkHasher func([]byte, []hash)
	matcher    func(a, b []byte, max int) int

	// compression algorithm
	fill func(*compressor, []byte) int // copy data to window
	step func(*compressor)             // process window
	sync bool                          // requesting flush

	// Input hash chains
	// hashHead[hashValue] contains the largest inputIndex with the specified hash value
	// If hashHead[hashValue] is within the current window, then
	// hashPrev[hashHead[hashValue] & windowMask] contains the previous index
	// with the same hash value.
	chainHead  int
	hashHead   []hashid
	hashPrev   []hashid
	hashOffset int

	// input window: unprocessed data is window[index:windowEnd]
	index         int
	window        []byte
	windowEnd     int
	blockStart    int  // window index where current tokens start
	byteAvailable bool // if true, still need to process window[index-1].

	// queued output tokens
	tokens tokens

	// deflate state
	length         int
	offset         int
	hash           hash
	maxInsertIndex int
	err            error

	hashMatch [maxMatchLength + minMatchLength]hash
}

type hash int32

func (d *compressor) fillDeflate(b []byte) int {
	if d.index >= 2*windowSize-(minMatchLength+maxMatchLength) {
		// shift the window by windowSize
		copy(d.window, d.window[windowSize:2*windowSize])
		d.index -= windowSize
		d.windowEnd -= windowSize
		if d.blockStart >= windowSize {
			d.blockStart -= windowSize
		} else {
			d.blockStart = math.MaxInt32
		}
		d.hashOffset += windowSize
		if d.hashOffset > maxHashOffset {
			delta := d.hashOffset - 1
			d.hashOffset -= delta
			d.chainHead -= delta
			for i, v := range d.hashPrev {
				if int(v) > delta {
					d.hashPrev[i] = hashid(int(v) - delta)
				} else {
					d.hashPrev[i] = 0
				}
			}
			for i, v := range d.hashHead {
				if int(v) > delta {
					d.hashHead[i] = hashid(int(v) - delta)
				} else {
					d.hashHead[i] = 0
				}
			}
		}
	}
	n := copy(d.window[d.windowEnd:], b)
	d.windowEnd += n
	return n
}

func (d *compressor) writeBlock(tok tokens, index int, eof bool) error {
	if index > 0 || eof {
		var window []byte
		if d.blockStart <= index {
			window = d.window[d.blockStart:index]
		}
		d.blockStart = index
		d.w.writeBlock(tok, eof, window)
		return d.w.err
	}
	return nil
}

// fillWindow will fill the current window with the supplied
// dictionary and calculate all hashes.
// This is much faster than doing a full encode.
// Should only be used after a start/reset.
func (d *compressor) fillWindow(b []byte) {
	// Any better way of finding if we are storing?
	if d.compressionLevel.good == 0 {
		return
	}
	// If we are given too much, cut it.
	if len(b) > windowSize {
		b = b[len(b)-windowSize:]
	}
	// Add all to window.
	n := copy(d.window[d.windowEnd:], b)

	// Calculate 256 hashes at the time (more L1 cache hits)
	loops := (n + 256 - minMatchLength) / 256
	for j := 0; j < loops; j++ {
		startindex := j * 256
		end := startindex + 256 + minMatchLength - 1
		if end > n {
			end = n
		}
		tocheck := d.window[startindex:end]
		dstSize := len(tocheck) - minMatchLength + 1

		if dstSize <= 0 {
			continue
		}

		dst := d.hashMatch[:dstSize]
		d.bulkHasher(tocheck, dst)
		var newH hash
		for i, val := range dst {
			di := i + startindex
			newH = val & hashMask
			// Get previous value with the same hash.
			// Our chain should point to the previous value.
			d.hashPrev[di&windowMask] = d.hashHead[newH]
			// Set the head of the hash chain to us.
			d.hashHead[newH] = hashid(di + d.hashOffset)
		}
		d.hash = newH
	}
	// Update window information.
	d.windowEnd += n
	d.index = n
}

// Try to find a match starting at index whose length is greater than prevSize.
// We only look at chainCount possibilities before giving up.
// pos = d.index, prevHead = d.chainHead-d.hashOffset, prevLength=minMatchLength-1, lookahead
func (d *compressor) findMatch(pos int, prevHead int, prevLength int, lookahead int) (length, offset int, ok bool) {
	minMatchLook := maxMatchLength
	if lookahead < minMatchLook {
		minMatchLook = lookahead
	}

	win := d.window[0 : pos+minMatchLook]

	// We quit when we get a match that's at least nice long
	nice := len(win) - pos
	if d.nice < nice {
		nice = d.nice
	}

	// If we've got a match that's good enough, only look in 1/4 the chain.
	tries := d.chain
	length = prevLength
	if length >= d.good {
		tries >>= 2
	}

	wEnd := win[pos+length]
	wPos := win[pos:]
	minIndex := pos - windowSize

	for i := prevHead; tries > 0; tries-- {
		if wEnd == win[i+length] {
			n := d.matcher(win[i:], wPos, minMatchLook)

			if n > length && (n > minMatchLength || pos-i <= 4096) {
				length = n
				offset = pos - i
				ok = true
				if n >= nice {
					// The match is good enough that we don't try to find a better one.
					break
				}
				wEnd = win[pos+n]
			}
		}
		if i == minIndex {
			// hashPrev[i & windowMask] has already been overwritten, so stop now.
			break
		}
		i = int(d.hashPrev[i&windowMask]) - d.hashOffset
		if i < minIndex || i < 0 {
			break
		}
	}
	return
}

func (d *compressor) writeStoredBlock(buf []byte) error {
	if d.w.writeStoredHeader(len(buf), false); d.w.err != nil {
		return d.w.err
	}
	d.w.writeBytes(buf)
	return d.w.err
}

func oldHash(b []byte) hash {
	return hash(b[0])<<(hashShift*3) + hash(b[1])<<(hashShift*2) + hash(b[2])<<hashShift + hash(b[3])
}

func oldBulkHash(b []byte, dst []hash) {
	if len(b) < minMatchLength {
		return
	}
	h := oldHash(b)
	dst[0] = h
	i := 1
	end := len(b) - minMatchLength + 1
	for ; i < end; i++ {
		h = (h << hashShift) + hash(b[i+3])
		dst[i] = h
	}
}

func matchLen(a, b []byte, max int) int {
	a = a[:max]
	for i, av := range a {
		if b[i] != av {
			return i
		}
	}
	return max
}

func (d *compressor) initDeflate() {
	d.hashHead = make([]hashid, hashSize)
	d.hashPrev = make([]hashid, windowSize)
	d.window = make([]byte, 2*windowSize)
	d.hashOffset = 1
	d.tokens.tokens = make([]token, maxFlateBlockTokens+1)
	d.length = minMatchLength - 1
	d.offset = 0
	d.byteAvailable = false
	d.index = 0
	d.hash = 0
	d.chainHead = -1
	d.hasher = oldHash
	d.bulkHasher = oldBulkHash
	d.matcher = matchLen
	if useSSE42 {
		d.hasher = crc32sse
		d.bulkHasher = crc32sseAll
		d.matcher = matchLenSSE4
	}
}

// Assumes that d.fastSkipHashing != skipNever,
// otherwise use deflateNoSkip
func (d *compressor) deflate() {

	// Sanity enables additional runtime tests.
	// It's intended to be used during development
	// to supplement the currently ad-hoc unit tests.
	const sanity = false

	if d.windowEnd-d.index < minMatchLength+maxMatchLength && !d.sync {
		return
	}

	d.maxInsertIndex = d.windowEnd - (minMatchLength - 1)
	if d.index < d.maxInsertIndex {
		d.hash = d.hasher(d.window[d.index:d.index+minMatchLength]) & hashMask
	}

Loop:
	for {
		if sanity && d.index > d.windowEnd {
			panic("index > windowEnd")
		}
		lookahead := d.windowEnd - d.index
		if lookahead < minMatchLength+maxMatchLength {
			if !d.sync {
				break Loop
			}
			if sanity && d.index > d.windowEnd {
				panic("index > windowEnd")
			}
			if lookahead == 0 {
				if d.tokens.n > 0 {
					if d.err = d.writeBlock(d.tokens, d.index, false); d.err != nil {
						return
					}
					d.tokens.n = 0
				}
				break Loop
			}
		}
		if d.index < d.maxInsertIndex {
			// Update the hash
			d.hash = d.hasher(d.window[d.index:d.index+minMatchLength]) & hashMask
			ch := d.hashHead[d.hash]
			d.chainHead = int(ch)
			d.hashPrev[d.index&windowMask] = ch
			d.hashHead[d.hash] = hashid(d.index + d.hashOffset)
		}
		d.length = minMatchLength - 1
		d.offset = 0
		minIndex := d.index - windowSize
		if minIndex < 0 {
			minIndex = 0
		}

		if d.chainHead-d.hashOffset >= minIndex && lookahead > minMatchLength-1 {
			if newLength, newOffset, ok := d.findMatch(d.index, d.chainHead-d.hashOffset, minMatchLength-1, lookahead); ok {
				d.length = newLength
				d.offset = newOffset
			}
		}
		if d.length >= minMatchLength {
			// There was a match at the previous step, and the current match is
			// not better. Output the previous match.
			// "d.length-3" should NOT be "d.length-minMatchLength", since the format always assume 3
			d.tokens.tokens[d.tokens.n] = matchToken(uint32(d.length-3), uint32(d.offset-minOffsetSize))
			d.tokens.n++
			// Insert in the hash table all strings up to the end of the match.
			// index and index-1 are already inserted. If there is not enough
			// lookahead, the last two strings are not inserted into the hash
			// table.
			if d.length <= d.fastSkipHashing {
				var newIndex int
				newIndex = d.index + d.length
				// Calculate missing hashes
				end := newIndex
				if end > d.maxInsertIndex {
					end = d.maxInsertIndex
				}
				end += minMatchLength - 1
				startindex := d.index + 1
				if startindex > d.maxInsertIndex {
					startindex = d.maxInsertIndex
				}
				tocheck := d.window[startindex:end]
				dstSize := len(tocheck) - minMatchLength + 1
				if dstSize > 0 {
					dst := d.hashMatch[:dstSize]
					d.bulkHasher(tocheck, dst)
					var newH hash
					for i, val := range dst {
						di := i + startindex
						newH = val & hashMask
						// Get previous value with the same hash.
						// Our chain should point to the previous value.
						d.hashPrev[di&windowMask] = d.hashHead[newH]
						// Set the head of the hash chain to us.
						d.hashHead[newH] = hashid(di + d.hashOffset)
					}
					d.hash = newH
				}

				d.index = newIndex

			} else {
				// For matches this long, we don't bother inserting each individual
				// item into the table.
				d.index += d.length
				if d.index < d.maxInsertIndex {
					d.hash = d.hasher(d.window[d.index:d.index+minMatchLength]) & hashMask
					//d.hash = (int(d.window[d.index])<<hashShift + int(d.window[d.index+1]))
				}
			}
			if d.tokens.n == maxFlateBlockTokens {
				// The block includes the current character
				if d.err = d.writeBlock(d.tokens, d.index, false); d.err != nil {
					return
				}
				d.tokens.n = 0
			}
		} else {
			i := d.index
			d.tokens.tokens[d.tokens.n] = literalToken(uint32(d.window[i]))
			d.tokens.n++
			if d.tokens.n == maxFlateBlockTokens {
				if d.err = d.writeBlock(d.tokens, i+1, false); d.err != nil {
					return
				}
				d.tokens.n = 0
			}
			d.index++
		}
	}
}

// same as deflate, but with d.fastSkipHashing == skipNever
func (d *compressor) deflateNoSkip() {
	// Sanity enables additional runtime tests.
	// It's intended to be used during development
	// to supplement the currently ad-hoc unit tests.
	const sanity = false

	if d.windowEnd-d.index < minMatchLength+maxMatchLength && !d.sync {
		return
	}

	d.maxInsertIndex = d.windowEnd - (minMatchLength - 1)
	if d.index < d.maxInsertIndex {
		d.hash = d.hasher(d.window[d.index:d.index+minMatchLength]) & hashMask
	}

Loop:
	for {
		if sanity && d.index > d.windowEnd {
			panic("index > windowEnd")
		}
		lookahead := d.windowEnd - d.index
		if lookahead < minMatchLength+maxMatchLength {
			if !d.sync {
				break Loop
			}
			if sanity && d.index > d.windowEnd {
				panic("index > windowEnd")
			}
			if lookahead == 0 {
				// Flush current output block if any.
				if d.byteAvailable {
					// There is still one pending token that needs to be flushed
					d.tokens.tokens[d.tokens.n] = literalToken(uint32(d.window[d.index-1]))
					d.tokens.n++
					d.byteAvailable = false
				}
				if d.tokens.n > 0 {
					if d.err = d.writeBlock(d.tokens, d.index, false); d.err != nil {
						return
					}
					d.tokens.n = 0
				}
				break Loop
			}
		}
		if d.index < d.maxInsertIndex {
			// Update the hash
			d.hash = d.hasher(d.window[d.index:d.index+minMatchLength]) & hashMask
			ch := d.hashHead[d.hash]
			d.chainHead = int(ch)
			d.hashPrev[d.index&windowMask] = ch
			d.hashHead[d.hash] = hashid(d.index + d.hashOffset)
		}
		prevLength := d.length
		prevOffset := d.offset
		d.length = minMatchLength - 1
		d.offset = 0
		minIndex := d.index - windowSize
		if minIndex < 0 {
			minIndex = 0
		}

		if d.chainHead-d.hashOffset >= minIndex && lookahead > prevLength && prevLength < d.lazy {
			if newLength, newOffset, ok := d.findMatch(d.index, d.chainHead-d.hashOffset, minMatchLength-1, lookahead); ok {
				d.length = newLength
				d.offset = newOffset
			}
		}
		if prevLength >= minMatchLength && d.length <= prevLength {
			// There was a match at the previous step, and the current match is
			// not better. Output the previous match.
			d.tokens.tokens[d.tokens.n] = matchToken(uint32(prevLength-3), uint32(prevOffset-minOffsetSize))
			d.tokens.n++
			// Insert in the hash table all strings up to the end of the match.
			// index and index-1 are already inserted. If there is not enough
			// lookahead, the last two strings are not inserted into the hash
			// table.
			var newIndex int
			newIndex = d.index + prevLength - 1
			// Calculate missing hashes
			end := newIndex
			if end > d.maxInsertIndex {
				end = d.maxInsertIndex
			}
			end += minMatchLength - 1
			startindex := d.index + 1
			if startindex > d.maxInsertIndex {
				startindex = d.maxInsertIndex
			}
			tocheck := d.window[startindex:end]
			dstSize := len(tocheck) - minMatchLength + 1
			if dstSize > 0 {
				dst := d.hashMatch[:dstSize]
				d.bulkHasher(tocheck, dst)
				var newH hash
				for i, val := range dst {
					di := i + startindex
					newH = val & hashMask
					// Get previous value with the same hash.
					// Our chain should point to the previous value.
					d.hashPrev[di&windowMask] = d.hashHead[newH]
					// Set the head of the hash chain to us.
					d.hashHead[newH] = hashid(di + d.hashOffset)
				}
				d.hash = newH
			}

			d.index = newIndex

			d.byteAvailable = false
			d.length = minMatchLength - 1
			if d.tokens.n == maxFlateBlockTokens {
				// The block includes the current character
				if d.err = d.writeBlock(d.tokens, d.index, false); d.err != nil {
					return
				}
				d.tokens.n = 0
			}
		} else {
			if d.byteAvailable {
				i := d.index - 1
				d.tokens.tokens[d.tokens.n] = literalToken(uint32(d.window[i]))
				d.tokens.n++
				if d.tokens.n == maxFlateBlockTokens {
					if d.err = d.writeBlock(d.tokens, i+1, false); d.err != nil {
						return
					}
					d.tokens.n = 0
				}
			}
			d.index++
			d.byteAvailable = true
		}
	}
}

func (d *compressor) fillStore(b []byte) int {
	n := copy(d.window[d.windowEnd:], b)
	d.windowEnd += n
	return n
}

func (d *compressor) store() {
	if d.windowEnd > 0 {
		d.err = d.writeStoredBlock(d.window[:d.windowEnd])
	}
	d.windowEnd = 0
}

func (d *compressor) fillHuff(b []byte) int {
	n := copy(d.window[d.windowEnd:], b)
	d.windowEnd += n
	return n
}

func (d *compressor) storeHuff() {
	// We only compress if we have >= 32KB (maxStoreBlockSize/2)
	if d.windowEnd < (maxStoreBlockSize/2) && !d.sync {
		return
	}
	if d.windowEnd == 0 {
		return
	}
	d.w.writeBlockHuff(false, d.window[:d.windowEnd])
	d.windowEnd = 0
}

func (d *compressor) storeSnappy() {
	// We only compress if we have >= 32KB (maxStoreBlockSize/2)
	if d.windowEnd < (maxStoreBlockSize/2) && !d.sync {
		return
	}
	if d.windowEnd == 0 {
		return
	}
	snappyEncode(&d.tokens, d.window[:d.windowEnd])
	d.w.writeBlock(d.tokens, false, d.window[:d.windowEnd])
	d.tokens.n = 0
	d.windowEnd = 0
}

func (d *compressor) write(b []byte) (n int, err error) {
	n = len(b)
	b = b[d.fill(d, b):]
	for len(b) > 0 {
		d.step(d)
		b = b[d.fill(d, b):]
	}
	return n, d.err
}

func (d *compressor) syncFlush() error {
	d.sync = true
	d.step(d)
	if d.err == nil {
		d.w.writeStoredHeader(0, false)
		d.w.flush()
		d.err = d.w.err
	}
	d.sync = false
	return d.err
}

func (d *compressor) init(w io.Writer, level int) (err error) {
	d.w = newHuffmanBitWriter(w)

	switch {
	case level == NoCompression:
		d.window = make([]byte, maxStoreBlockSize)
		d.fill = (*compressor).fillStore
		d.step = (*compressor).store
	case level == ConstantCompression:
		d.window = make([]byte, maxStoreBlockSize)
		d.fill = (*compressor).fillHuff
		d.step = (*compressor).storeHuff
	case level == 1:
		d.window = make([]byte, maxStoreBlockSize)
		d.fill = (*compressor).fillHuff
		d.step = (*compressor).storeSnappy
		d.tokens.tokens = make([]token, maxStoreBlockSize+1)
	case level == DefaultCompression:
		level = 6
		fallthrough
	case 2 <= level && level <= 9:
		d.compressionLevel = levels[level]
		d.initDeflate()
		d.fill = (*compressor).fillDeflate
		if d.fastSkipHashing == skipNever {
			d.step = (*compressor).deflateNoSkip
		} else {
			d.step = (*compressor).deflate
		}
	default:
		return fmt.Errorf("flate: invalid compression level %d: want value in range [-2, 9]", level)
	}
	return nil
}

var zeroes [64]int
var hzeroes [256]hashid
var bzeroes [256]byte

func (d *compressor) reset(w io.Writer) {
	d.w.reset(w)
	d.sync = false
	d.err = nil
	switch d.compressionLevel.chain {
	case 0:
		// level was NoCompression or ConstantCompresssion.
		d.windowEnd = 0
	default:
		d.chainHead = -1
		for s := d.hashHead; len(s) > 0; {
			n := copy(s, hzeroes[:])
			s = s[n:]
		}
		for s := d.hashPrev; len(s) > 0; s = s[len(hzeroes):] {
			copy(s, hzeroes[:])
		}
		d.hashOffset = 1

		d.index, d.windowEnd = 0, 0
		d.blockStart, d.byteAvailable = 0, false

		d.tokens.n = 0
		d.length = minMatchLength - 1
		d.offset = 0
		d.hash = 0
		d.maxInsertIndex = 0
	}
}

func (d *compressor) close() error {
	d.sync = true
	d.step(d)
	if d.err != nil {
		return d.err
	}
	if d.w.writeStoredHeader(0, true); d.w.err != nil {
		return d.w.err
	}
	d.w.flush()
	return d.w.err
}

// NewWriter returns a new Writer compressing data at the given level.
// Following zlib, levels range from 1 (BestSpeed) to 9 (BestCompression);
// higher levels typically run slower but compress more. Level 0
// (NoCompression) does not attempt any compression; it only adds the
// necessary DEFLATE framing. Level -1 (DefaultCompression) uses the default
// compression level.
//
// If level is in the range [-1, 9] then the error returned will be nil.
// Otherwise the error returned will be non-nil.
func NewWriter(w io.Writer, level int) (*Writer, error) {
	var dw Writer
	if err := dw.d.init(w, level); err != nil {
		return nil, err
	}
	return &dw, nil
}

// NewWriterDict is like NewWriter but initializes the new
// Writer with a preset dictionary.  The returned Writer behaves
// as if the dictionary had been written to it without producing
// any compressed output.  The compressed data written to w
// can only be decompressed by a Reader initialized with the
// same dictionary.
func NewWriterDict(w io.Writer, level int, dict []byte) (*Writer, error) {
	dw := &dictWriter{w}
	zw, err := NewWriter(dw, level)
	if err != nil {
		return nil, err
	}
	zw.d.fillWindow(dict)
	zw.dict = append(zw.dict, dict...) // duplicate dictionary for Reset method.
	return zw, err
}

type dictWriter struct {
	w io.Writer
}

func (w *dictWriter) Write(b []byte) (n int, err error) {
	return w.w.Write(b)
}

// A Writer takes data written to it and writes the compressed
// form of that data to an underlying writer (see NewWriter).
type Writer struct {
	d    compressor
	dict []byte
}

// Write writes data to w, which will eventually write the
// compressed form of data to its underlying writer.
func (w *Writer) Write(data []byte) (n int, err error) {
	return w.d.write(data)
}

// Flush flushes any pending compressed data to the underlying writer.
// It is useful mainly in compressed network protocols, to ensure that
// a remote reader has enough data to reconstruct a packet.
// Flush does not return until the data has been written.
// If the underlying writer returns an error, Flush returns that error.
//
// In the terminology of the zlib library, Flush is equivalent to Z_SYNC_FLUSH.
func (w *Writer) Flush() error {
	// For more about flushing:
	// http://www.bolet.org/~pornin/deflate-flush.html
	return w.d.syncFlush()
}

// Close flushes and closes the writer.
func (w *Writer) Close() error {
	return w.d.close()
}

// Reset discards the writer's state and makes it equivalent to
// the result of NewWriter or NewWriterDict called with dst
// and w's level and dictionary.
func (w *Writer) Reset(dst io.Writer) {
	if dw, ok := w.d.w.w.(*dictWriter); ok {
		// w was created with NewWriterDict
		dw.w = dst
		w.d.reset(dw)
		w.d.fillWindow(w.dict)
	} else {
		// w was created with NewWriter
		w.d.reset(dst)
	}
}

// ResetDict discards the writer's state and makes it equivalent to
// the result of NewWriter or NewWriterDict called with dst
// and w's level, but sets a specific dictionary.
func (w *Writer) ResetDict(dst io.Writer, dict []byte) {
	w.dict = dict
	w.d.reset(dst)
	w.d.fillWindow(w.dict)
}
