package ctlog

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"filippo.io/litetlog/internal/tlogx"
	ct "github.com/google/certificate-transparency-go"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
	"golang.org/x/sync/errgroup"
)

type Log struct {
	name    string
	logID   [sha256.Size]byte
	privKey crypto.Signer
	backend Backend

	// TODO: add a lock when using these outside the sequencer.
	tree treeWithTimestamp
	// edgeTiles is a map from level to the right-most tile of that level.
	edgeTiles map[int]tileWithBytes

	// poolMu is held for the entire duration of addLeafToPool, and by
	// sequencePool while swapping the pool. This guarantees that addLeafToPool
	// will never add to a pool that already started sequencing.
	poolMu      sync.Mutex
	currentPool *pool
}

type treeWithTimestamp struct {
	tlog.Tree
	Time int64
}

type tileWithBytes struct {
	tlog.Tile
	B []byte
}

func CreateLog(ctx context.Context, name string, key crypto.Signer, backend Backend) error {
	pkix, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return err
	}
	logID := sha256.Sum256(pkix)

	tree := treeWithTimestamp{tlog.Tree{}, timeNowUnixMilli()}
	checkpoint, err := signTreeHead(name, logID, key, tree)
	if err != nil {
		return err
	}

	return backend.Upload(ctx, "sth", checkpoint)
}

func LoadLog(ctx context.Context, name string, key crypto.Signer, backend Backend) (*Log, error) {
	pkix, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, err
	}
	logID := sha256.Sum256(pkix)

	sth, err := backend.Fetch(ctx, "sth")
	if err != nil {
		return nil, err
	}
	v, err := tlogx.NewRFC6962Verifier(name, key.Public())
	if err != nil {
		return nil, err
	}
	var timestamp int64
	v.Timestamp = func(t uint64) { timestamp = int64(t) }
	n, err := note.Open(sth, note.VerifierList(v))
	if err != nil {
		return nil, err
	}
	c, err := tlogx.ParseCheckpoint(n.Text)
	if err != nil {
		return nil, err
	}

	if now := timeNowUnixMilli(); now < timestamp {
		return nil, fmt.Errorf("current time %d is before STH time %d", now, timestamp)
	}
	if c.Origin != name {
		return nil, fmt.Errorf("STH name is %q, not %q", c.Origin, name)
	}
	tree := tlog.Tree{N: c.N, Hash: c.Hash}
	if c.Extension != "" {
		return nil, fmt.Errorf("unexpected STH extension %q", c.Extension)
	}

	edgeTiles := make(map[int]tileWithBytes)
	if c.N > 0 {
		if _, err := tlog.TileHashReader(tree, &tileReader{backend, func(tiles []tlog.Tile, data [][]byte) {
			for i, tile := range tiles {
				if t, ok := edgeTiles[tile.L]; !ok || t.N < tile.N || (t.N == tile.N && t.W < tile.W) {
					edgeTiles[tile.L] = tileWithBytes{tile, data[i]}
				}
			}
		}}).ReadHashes([]int64{tlog.StoredHashIndex(0, c.N-1)}); err != nil {
			return nil, err
		}

		dataTile := edgeTiles[0]
		dataTile.L = -1
		dataTile.B, err = backend.Fetch(context.Background(), dataTile.Path())
		if err != nil {
			return nil, err
		}
		edgeTiles[-1] = dataTile

		b := edgeTiles[-1].B
		start := tileWidth * dataTile.N
		for i := start; i < start+int64(dataTile.W); i++ {
			timestampedEntry, _, _, rest, err := ReadTileLeaf(b)
			if err != nil {
				return nil, fmt.Errorf("invalid data tile %v", dataTile)
			}
			b = rest

			got := tlog.RecordHash(append([]byte{0, 0}, timestampedEntry...))
			exp, err := tlog.HashFromTile(edgeTiles[0].Tile, edgeTiles[0].B, tlog.StoredHashIndex(0, i))
			if err != nil {
				return nil, err
			}
			if got != exp {
				return nil, fmt.Errorf("tile leaf entry %d hashes to %v, level 0 hash is %v", i, got, exp)
			}
		}
	}

	return &Log{
		name:        name,
		logID:       logID,
		privKey:     key,
		backend:     backend,
		tree:        treeWithTimestamp{tree, timestamp},
		edgeTiles:   edgeTiles,
		currentPool: &pool{done: make(chan struct{})},
	}, nil
}

var timeNowUnixMilli = func() int64 { return time.Now().UnixMilli() }

// Backend is a strongly consistent object storage.
type Backend interface {
	// Upload is expected to retry transient errors, and only return an error
	// for unrecoverable errors. When Upload returns, the object must be fully
	// persisted. Upload can be called concurrently.
	Upload(ctx context.Context, key string, data []byte) error

	// Fetch can be called concurrently.
	Fetch(ctx context.Context, key string) ([]byte, error)
}

const tileHeight = 10
const tileWidth = 1 << tileHeight

type tileReader struct {
	Backend
	saveTiles func(tiles []tlog.Tile, data [][]byte)
}

func (r *tileReader) Height() int {
	return tileHeight
}

func (r *tileReader) ReadTiles(tiles []tlog.Tile) (data [][]byte, err error) {
	for _, t := range tiles {
		b, err := r.Backend.Fetch(context.Background(), t.Path())
		if err != nil {
			return nil, err
		}
		data = append(data, b)
	}
	return data, nil
}

func (r *tileReader) SaveTiles(tiles []tlog.Tile, data [][]byte) { r.saveTiles(tiles, data) }

type logEntry struct {
	// cert is either the x509_entry or the tbs_certificate for precerts.
	cert []byte

	isPrecert          bool
	issuerKeyHash      [32]byte
	preCertificate     []byte
	precertSigningCert []byte
}

// merkleTreeLeaf returns a RFC 6962 MerkleTreeLeaf.
func (e *logEntry) merkleTreeLeaf(timestamp int64) []byte {
	b := &cryptobyte.Builder{}
	b.AddUint8(0 /* version = v1 */)
	b.AddUint8(0 /* leaf_type = timestamped_entry */)
	e.timestampedEntry(b, timestamp)
	return b.BytesOrPanic()
}

func (e *logEntry) timestampedEntry(b *cryptobyte.Builder, timestamp int64) {
	b.AddUint64(uint64(timestamp))
	if !e.isPrecert {
		b.AddUint8(0 /* entry_type = x509_entry */)
		b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddBytes(e.cert)
		})
	} else {
		b.AddUint8(1 /* entry_type = precert_entry */)
		b.AddBytes(e.issuerKeyHash[:])
		b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddBytes(e.cert)
		})
	}
	b.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
		/* extensions */
	})
}

func (e *logEntry) tileLeaf(timestamp int64) []byte {
	// struct {
	//     TimestampedEntry timestamped_entry;
	//     select(entry_type) {
	//         case x509_entry: Empty;
	//         case precert_entry: PreCertExtraData;
	//     } extra_data;
	// } TileLeaf;
	//
	// struct {
	//     ASN.1Cert pre_certificate;
	//     opaque PrecertificateSigningCertificate<0..2^24-1>;
	// } PreCertExtraData;

	b := &cryptobyte.Builder{}
	e.timestampedEntry(b, timestamp)
	if e.isPrecert {
		b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddBytes(e.preCertificate)
		})
		b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddBytes(e.precertSigningCert)
		})
	}
	return b.BytesOrPanic()
}

type pool struct {
	pendingLeaves []*logEntry

	// done is closed when the pool has been sequenced and
	// the results below are ready.
	done chan struct{}

	// firstLeafIndex is the 0-based index of pendingLeaves[0] in the tree, and
	// every following entry is sequenced contiguously.
	firstLeafIndex int64
	// timestamp is both the STH and the SCT timestamp.
	// "The timestamp MUST be at least as recent as the most recent SCT
	// timestamp in the tree." RFC 6962, Section 3.5.
	timestamp int64
}

// addLeafToPool adds leaf to the current pool, and returns a function that will
// wait until the pool is sequenced and returns the index of the leaf.
func (l *Log) addLeafToPool(leaf *logEntry) func() (id int64) {
	l.poolMu.Lock()
	defer l.poolMu.Unlock()
	p := l.currentPool
	n := len(p.pendingLeaves)
	p.pendingLeaves = append(p.pendingLeaves, leaf)
	return func() int64 {
		<-p.done
		return p.firstLeafIndex + int64(n)
	}
}

const sequenceTimeout = 5 * time.Second

func (l *Log) sequencePool() error {
	l.poolMu.Lock()
	p := l.currentPool
	l.currentPool = &pool{done: make(chan struct{})}
	l.poolMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), sequenceTimeout)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	defer g.Wait()

	timestamp := timeNowUnixMilli()
	if timestamp <= l.tree.Time {
		return fmt.Errorf("time did not progress! %d -> %d", l.tree.Time, timestamp)
	}

	edgeTiles := maps.Clone(l.edgeTiles)
	var dataTile []byte
	if t, ok := edgeTiles[-1]; ok && t.W < tileWidth {
		dataTile = bytes.Clone(t.B)
	}
	newHashes := make(map[int64]tlog.Hash)
	hashReader := l.hashReader(newHashes)
	n := l.tree.N
	for _, leaf := range p.pendingLeaves {
		hashes, err := tlog.StoredHashes(n, leaf.merkleTreeLeaf(timestamp), hashReader)
		if err != nil {
			return fmt.Errorf("couldn't fetch stored hashes for leaf %d: %w", n, err)
		}
		for i, h := range hashes {
			id := tlog.StoredHashIndex(0, n) + int64(i)
			newHashes[id] = h
		}
		dataTile = append(dataTile, leaf.tileLeaf(timestamp)...)

		n++

		if n%tileWidth == 0 { // Data tile is full.
			tile := tlog.TileForIndex(tileHeight, tlog.StoredHashIndex(0, n-1))
			tile.L = -1
			data := dataTile
			edgeTiles[-1] = tileWithBytes{tile, data}
			g.Go(func() error { return l.backend.Upload(gctx, tile.Path(), data) })
			dataTile = nil
		}
	}

	// Upload partial data tile.
	if n%tileWidth != 0 {
		tile := tlog.TileForIndex(tileHeight, tlog.StoredHashIndex(0, n-1))
		tile.L = -1
		edgeTiles[-1] = tileWithBytes{tile, dataTile}
		g.Go(func() error { return l.backend.Upload(gctx, tile.Path(), dataTile) })
	}

	tiles := tlog.NewTiles(tileHeight, l.tree.N, n)
	for _, tile := range tiles {
		data, err := tlog.ReadTileData(tile, hashReader)
		if err != nil {
			return err
		}
		tile := tile
		if t, ok := edgeTiles[tile.L]; !ok || t.N < tile.N || (t.N == tile.N && t.W < tile.W) {
			edgeTiles[tile.L] = tileWithBytes{tile, data}
		}
		g.Go(func() error { return l.backend.Upload(gctx, tile.Path(), data) })
	}

	if err := g.Wait(); err != nil {
		return err
	}

	rootHash, err := tlog.TreeHash(n, hashReader)
	if err != nil {
		return err
	}
	tree := treeWithTimestamp{Tree: tlog.Tree{N: n, Hash: rootHash}, Time: timestamp}

	checkpoint, err := signTreeHead(l.name, l.logID, l.privKey, tree)
	if err != nil {
		return err
	}
	if err := l.backend.Upload(ctx, "sth", checkpoint); err != nil {
		// TODO: this is a critical error to handle, since if the STH actually
		// got committed before the error we need to make very very sure we
		// don't sign an inconsistent version when we retry.
		return err
	}

	defer close(p.done)
	p.timestamp = timestamp
	p.firstLeafIndex = l.tree.N
	l.tree = tree
	l.edgeTiles = edgeTiles

	return nil
}

// signTreeHead signs the tree and returns a checkpoint according to
// c2sp.org/checkpoint.
func signTreeHead(name string, logID [sha256.Size]byte, privKey crypto.Signer, tree treeWithTimestamp) (checkpoint []byte, err error) {
	sthBytes, err := ct.SerializeSTHSignatureInput(ct.SignedTreeHead{
		Version:        ct.V1,
		TreeSize:       uint64(tree.N),
		Timestamp:      uint64(tree.Time),
		SHA256RootHash: ct.SHA256Hash(tree.Hash),
	})
	if err != nil {
		return nil, err
	}

	// We compute the signature here and inject it in a fixed note.Signer to
	// avoid a risky serialize-deserialize loop, and to control the timestamp.

	treeHeadSignature, err := digitallySign(privKey, sthBytes)
	if err != nil {
		return nil, err
	}

	// struct {
	//     uint64 timestamp;
	//     TreeHeadSignature signature;
	// } RFC6962NoteSignature;
	var b cryptobyte.Builder
	b.AddUint64(uint64(tree.Time))
	b.AddBytes(treeHeadSignature)
	sig, err := b.Bytes()
	if err != nil {
		return nil, err
	}

	signer, err := tlogx.NewInjectedSigner(name, 0x05, logID[:], sig)
	if err != nil {
		return nil, err
	}
	return note.Sign(&note.Note{
		Text: tlogx.MarshalCheckpoint(tlogx.Checkpoint{
			Origin: name,
			N:      tree.N, Hash: tree.Hash,
		}),
	}, signer)
}

// digitallySign produces an encoded digitally-signed signature.
//
// It reimplements tls.CreateSignature and tls.Marshal from
// github.com/google/certificate-transparency-go/tls, in part to limit
// complexity and in part because tls.CreateSignature expects non-pointer
// {rsa,ecdsa}.PrivateKey types, which is unusual.
func digitallySign(k crypto.Signer, msg []byte) ([]byte, error) {
	h := sha256.Sum256(msg)
	sig, err := k.Sign(rand.Reader, h[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}
	var b cryptobyte.Builder
	b.AddUint8(4 /* hash = sha256 */)
	switch k.Public().(type) {
	case *rsa.PublicKey:
		b.AddUint8(1 /* signature = rsa */)
	case *ecdsa.PublicKey:
		b.AddUint8(3 /* signature = ecdsa */)
	default:
		return nil, fmt.Errorf("unsupported key type %T", k.Public())
	}
	b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddBytes(sig)
	})
	return b.Bytes()
}

func (l *Log) hashReader(overlay map[int64]tlog.Hash) tlog.HashReaderFunc {
	return func(indexes []int64) ([]tlog.Hash, error) {
		list := make([]tlog.Hash, 0, len(indexes))
		for _, id := range indexes {
			if h, ok := overlay[id]; ok {
				list = append(list, h)
				continue
			}
			t := l.edgeTiles[tlog.TileForIndex(tileHeight, id).L]
			h, err := tlog.HashFromTile(t.Tile, t.B, id)
			if err != nil {
				return nil, err
			}
			list = append(list, h)
		}
		return list, nil
	}
}

func ReadTileLeaf(tile []byte) (timestampedEntry, cert []byte, timestamp uint64, rest []byte, err error) {
	// TODO: make a return type for this and merge it with logEntry.

	s := cryptobyte.String(tile)
	var entryType uint8
	var extensions []byte
	if !s.ReadUint64(&timestamp) || !s.ReadUint8(&entryType) {
		err = errors.New("invalid data tile")
		return
	}
	switch entryType {
	case 0: // x509_entry
		if !s.ReadUint24LengthPrefixed((*cryptobyte.String)(&cert)) {
			err = errors.New("invalid data tile")
			return
		}
	case 1: // precert_entry
		panic("unimplemented") // TODO
	default:
		err = fmt.Errorf("invalid data tile %v: unknown type %d", tile, entryType)
		return
	}
	if !s.ReadUint16LengthPrefixed((*cryptobyte.String)(&extensions)) {
		err = errors.New("invalid data tile")
		return
	}
	// TODO: parse and handle extensions.

	timestampedEntry = tile[:len(tile)-len(s)]
	rest = s
	return
}