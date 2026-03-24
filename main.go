package main

import (
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/corona10/goimagehash"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Pipeline:
// Images → Perceptual hash → LSH bucket → BK-tree search trong bucket → Hamming filter → Group → Keep largest → Delete → Log

const normalizeMaxSide = 256
const progressStep = 200

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
}

type imageRecord struct {
	Path   string
	Width  int
	Height int
	Pixels int64
	Hash   *goimagehash.ImageHash
	Err    error
}

func main() {
	root := flag.String("root", ".", "Thư mục gốc chứa ảnh (quét đệ quy)")
	maxDist := flag.Int("distance", 10, "Ngưỡng khoảng cách Hamming giữa hai perceptual hash (càng nhỏ càng khắt khe)")
	lshBits := flag.Int("lsh-bits", 2, "Số bit mỗi band LSH (1,2,4,8,16 — phải chia hết 64). Band càng nhỏ càng nhiều bucket, an toàn với -distance lớn")
	dryRun := flag.Bool("dry-run", false, "Chỉ ghi log, không xóa file")
	logPath := flag.String("log", "dedupe_design.log", "Đường dẫn file log")
	flag.Parse()

	// Hỗ trợ truyền nhanh thư mục gốc dưới dạng positional argument:
	// dedupe.exe "D:\\Designs"
	if flag.NArg() > 0 {
		*root = flag.Arg(0)
	}

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("abs root: %v", err)
	}
	startAll := time.Now()
	log.Printf("[INIT] root=%s distance=%d lsh-bits=%d dry-run=%v", absRoot, *maxDist, *lshBits, *dryRun)

	records := scanImages(absRoot)
	log.Printf("[SCAN] Total records: %d", len(records))
	var ok []*imageRecord
	for _, r := range records {
		if r.Err != nil {
			log.Printf("skip %s: %v", r.Path, r.Err)
			continue
		}
		ok = append(ok, r)
	}
	if len(ok) == 0 {
		log.Println("Không có ảnh hợp lệ.")
		return
	}

	if err := validateLSHRecall(*lshBits, *maxDist); err != nil {
		log.Fatal(err)
	}
	log.Printf("[VALIDATE] LSH recall rule passed")

	hashes := make([]uint64, len(ok))
	for i, r := range ok {
		hashes[i] = r.Hash.GetHash()
	}

	log.Printf("[INDEX 1/3] Build LSH buckets + BK-tree roots...")
	t0 := time.Now()
	roots := buildLSHRoots(hashes, *lshBits)
	log.Printf("[INDEX 1/3] Done: buckets=%d, elapsed=%s", len(roots), time.Since(t0).Round(time.Millisecond))

	// Union-find: nối các ảnh có Hamming ≤ maxDist (ứng viên từ LSH + BK-tree).
	parent := make([]int, len(ok))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}

	log.Printf("[INDEX 2/3] Search neighbors with BK-tree in each LSH bucket...")
	t1 := time.Now()
	unionNeighborsLSHBK(hashes, *lshBits, *maxDist, roots, union)
	log.Printf("[INDEX 2/3] Done: elapsed=%s", time.Since(t1).Round(time.Millisecond))

	log.Printf("[INDEX 3/3] Build final groups...")
	groups := make(map[int][]int)
	for i := range ok {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	log.Printf("[INDEX 3/3] Done: groups=%d", len(groups))

	logFile, err := os.Create(*logPath)
	if err != nil {
		log.Fatalf("tạo log: %v", err)
	}
	defer logFile.Close()

	ts := time.Now().Format(time.RFC3339)
	fmt.Fprintf(logFile, "=== Dedupe design log %s ===\n", ts)
	numBands := 64 / *lshBits
	fmt.Fprintf(logFile, "root=%s max_hamming=%d lsh_bits_per_band=%d lsh_bands=%d dry_run=%v\n\n",
		absRoot, *maxDist, *lshBits, numBands, *dryRun)

	deleted := 0
	kept := 0
	dupGroups := make([][]int, 0, len(groups))
	for _, idxs := range groups {
		if len(idxs) > 1 {
			dupGroups = append(dupGroups, idxs)
		}
	}
	log.Printf("[DELETE] Duplicate groups=%d", len(dupGroups))

	for gi, idxs := range dupGroups {
		sort.Slice(idxs, func(a, b int) bool {
			pa := ok[idxs[a]].Pixels
			pb := ok[idxs[b]].Pixels
			if pa != pb {
				return pa > pb
			}
			return ok[idxs[a]].Path < ok[idxs[b]].Path
		})
		keep := idxs[0]
		rest := idxs[1:]
		fmt.Fprintf(logFile, "GROUP (%d ảnh tương tự):\n", len(idxs))
		fmt.Fprintf(logFile, "  KEEP: %s (%dx%d)\n", ok[keep].Path, ok[keep].Width, ok[keep].Height)
		for _, ri := range rest {
			fmt.Fprintf(logFile, "  DEL:  %s (%dx%d)\n", ok[ri].Path, ok[ri].Width, ok[ri].Height)
			if !*dryRun {
				if err := os.Remove(ok[ri].Path); err != nil {
					fmt.Fprintf(logFile, "  ERROR remove: %v\n", err)
					log.Printf("xóa %s: %v", ok[ri].Path, err)
				} else {
					deleted++
				}
			} else {
				deleted++
			}
		}
		fmt.Fprintln(logFile)
		kept++
		if shouldLogProgress(gi+1, len(dupGroups), 20) {
			log.Printf("[DELETE] Progress groups: %d/%d (%.1f%%)", gi+1, len(dupGroups), percent(gi+1, len(dupGroups)))
		}
	}

	fmt.Fprintf(logFile, "--- Tổng kết: %d nhóm có trùng, giữ 1 ảnh/nhóm, %d file %s ---\n",
		kept, deleted, map[bool]string{true: "sẽ xóa (dry-run)", false: "đã xóa"}[*dryRun])

	log.Printf("[DONE] Elapsed=%s, log=%s", time.Since(startAll).Round(time.Millisecond), *logPath)
}

func scanImages(root string) []*imageRecord {
	log.Printf("[SCAN 1/2] Discover image files...")
	var paths []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !imageExts[ext] {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	log.Printf("[SCAN 1/2] Found %d images", len(paths))

	out := make([]*imageRecord, 0, len(paths))
	if len(paths) == 0 {
		return out
	}
	log.Printf("[SCAN 2/2] Decode + normalize + hash...")
	t0 := time.Now()
	for i, path := range paths {
		out = append(out, processImage(path))
		if shouldLogProgress(i+1, len(paths), progressStep) {
			log.Printf("[SCAN 2/2] Progress: %d/%d (%.1f%%)", i+1, len(paths), percent(i+1, len(paths)))
		}
	}
	log.Printf("[SCAN 2/2] Done in %s", time.Since(t0).Round(time.Millisecond))
	return out
}

func shouldLogProgress(done, total, step int) bool {
	if total <= 0 {
		return false
	}
	if done == total {
		return true
	}
	if step <= 0 {
		return false
	}
	return done%step == 0
}

func percent(done, total int) float64 {
	if total == 0 {
		return 100
	}
	return float64(done) * 100 / float64(total)
}

func processImage(path string) *imageRecord {
	r := &imageRecord{Path: path}
	f, err := os.Open(path)
	if err != nil {
		r.Err = err
		return r
	}
	defer f.Close()

	img, format, err := decodeImage(f)
	if err != nil {
		r.Err = fmt.Errorf("decode: %w", err)
		return r
	}

	b := img.Bounds()
	r.Width, r.Height = b.Dx(), b.Dy()
	r.Pixels = int64(r.Width) * int64(r.Height)

	norm := normalizeImage(img)
	h, err := goimagehash.PerceptionHash(norm)
	if err != nil {
		r.Err = fmt.Errorf("phash: %w", err)
		return r
	}
	_ = format
	r.Hash = h
	return r
}

func decodeImage(r io.Reader) (image.Image, string, error) {
	// Peek first bytes for format sniff if needed; std Decode handles most.
	return image.Decode(r)
}

// normalizeImage: scale ảnh sao cho cạnh dài nhất = normalizeMaxSide, giữ tỷ lệ (bước "Normalize" trong pipeline).
func normalizeImage(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return src
	}
	maxW, maxH := w, h
	if maxW < maxH {
		maxW, maxH = maxH, maxW
	}
	scale := float64(normalizeMaxSide) / float64(maxW)
	nw := int(float64(w)*scale + 0.5)
	nh := int(float64(h)*scale + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// --- LSH + BK-tree (Hamming) ---

type bkNode struct {
	hash uint64
	id   int
	kids map[int]*bkNode
}

func bkInsert(n *bkNode, hash uint64, id int) *bkNode {
	if n == nil {
		return &bkNode{hash: hash, id: id, kids: make(map[int]*bkNode)}
	}
	d := hamming64(n.hash, hash)
	n.kids[d] = bkInsert(n.kids[d], hash, id)
	return n
}

func bkSearch(n *bkNode, q uint64, r int, out *[]int) {
	if n == nil {
		return
	}
	d := hamming64(n.hash, q)
	if d <= r {
		*out = append(*out, n.id)
	}
	for cd, ch := range n.kids {
		if hammingTriOk(cd, d, r) {
			bkSearch(ch, q, r, out)
		}
	}
}

func hammingTriOk(cd, d, r int) bool {
	x := cd - d
	if x < 0 {
		x = -x
	}
	return x <= r
}

func hamming64(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

func bandSegment(h uint64, bandIdx, bitsPerBand int) uint64 {
	shift := uint(bandIdx * bitsPerBand)
	mask := (uint64(1) << uint(bitsPerBand)) - 1
	return (h >> shift) & mask
}

func lshKey(bandIdx int, seg uint64) uint32 {
	return uint32(bandIdx)<<8 | uint32(seg)
}

func buildLSHRoots(hashes []uint64, bitsPerBand int) map[uint32]*bkNode {
	if 64%bitsPerBand != 0 {
		panic("lsh: bitsPerBand must divide 64")
	}
	numBands := 64 / bitsPerBand
	bucketLists := make(map[uint32][]int)
	for i, h := range hashes {
		for b := 0; b < numBands; b++ {
			seg := bandSegment(h, b, bitsPerBand)
			key := lshKey(b, seg)
			bucketLists[key] = append(bucketLists[key], i)
		}
	}
	roots := make(map[uint32]*bkNode, len(bucketLists))
	for key, ids := range bucketLists {
		var root *bkNode
		for _, idx := range ids {
			root = bkInsert(root, hashes[idx], idx)
		}
		roots[key] = root
	}
	return roots
}

func unionNeighborsLSHBK(
	hashes []uint64,
	bitsPerBand, maxDist int,
	roots map[uint32]*bkNode,
	union func(a, b int),
) {
	n := len(hashes)
	if n == 0 {
		return
	}
	numBands := 64 / bitsPerBand
	stamp := make([]int, n)
	token := 0
	var searchBuf []int
	var cands []int

	for i := 0; i < n; i++ {
		token++
		cands = cands[:0]
		hi := hashes[i]
		for b := 0; b < numBands; b++ {
			seg := bandSegment(hi, b, bitsPerBand)
			key := lshKey(b, seg)
			root := roots[key]
			if root == nil {
				continue
			}
			searchBuf = searchBuf[:0]
			bkSearch(root, hi, maxDist, &searchBuf)
			for _, j := range searchBuf {
				if j == i {
					continue
				}
				if stamp[j] != token {
					stamp[j] = token
					cands = append(cands, j)
				}
			}
		}
		for _, j := range cands {
			if hamming64(hi, hashes[j]) <= maxDist {
				union(i, j)
			}
		}
	}
}

func validateLSHRecall(bitsPerBand, maxDist int) error {
	if bitsPerBand < 1 || bitsPerBand > 16 {
		return fmt.Errorf("lsh-bits phải trong [1,16] và chia hết 64")
	}
	if 64%bitsPerBand != 0 {
		return fmt.Errorf("lsh-bits phải là ước của 64 (1,2,4,8,16)")
	}
	numBands := 64 / bitsPerBand
	if maxDist >= numBands {
		return fmt.Errorf(
			"để LSH không bỏ sót cặp trùng: số band (%d) phải > max Hamming (%d); giảm -distance hoặc giảm -lsh-bits (vd. 1 => 64 band)",
			numBands, maxDist,
		)
	}
	return nil
}
