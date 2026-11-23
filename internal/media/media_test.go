package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func jpegBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

func pngBytes(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestProcessJPEG(t *testing.T) {
	p, err := Process(jpegBytes(320, 200))
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 320 || p.Height != 200 {
		t.Fatalf("dims %dx%d", p.Width, p.Height)
	}
	if p.Ext != "jpg" || p.ContentType != "image/jpeg" {
		t.Fatalf("type %s/%s", p.Ext, p.ContentType)
	}
	if !strings.HasPrefix(p.Key, "media/") || !strings.HasSuffix(p.Key, ".jpg") {
		t.Fatalf("key shape: %s", p.Key)
	}
	if len(p.SHA256) != 64 || !strings.Contains(p.Key, p.SHA256[:2]+"/"+p.SHA256) {
		t.Fatalf("content addressing broken: %s vs %s", p.Key, p.SHA256)
	}
}

func TestProcessPNGDeterministicKey(t *testing.T) {
	raw := pngBytes(100, 150)
	a, err := Process(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Process(raw)
	if a.Key != b.Key {
		t.Fatal("content-addressed keys must be deterministic")
	}
	if a.Width != 100 || a.Height != 150 || a.Ext != "png" {
		t.Fatalf("png: %+v", a)
	}
}

func TestProcessRejectsNonImages(t *testing.T) {
	for _, junk := range [][]byte{[]byte("not an image"), {}, []byte("<html>")[:]} {
		if _, err := Process(junk); err == nil {
			t.Fatalf("junk accepted: %q", junk)
		}
	}
}

// hugePNG declares gigantic dimensions in its IHDR and carries no IDAT —
// png.DecodeConfig reads only the header, which is exactly what the guard
// must confront WITHOUT allocating pixels first (a real 100 MP fixture
// would itself be the memory problem the guard exists to prevent).
func hugePNG(width, height uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 6  // color type: RGBA
	ihdr[12] = 0 // no interlace

	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) // signature
	var chunk bytes.Buffer
	binary.Write(&chunk, binary.BigEndian, uint32(len(ihdr)))
	chunk.WriteString("IHDR")
	chunk.Write(ihdr)
	binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(chunk.Bytes()[4:]))
	out.Write(chunk.Bytes())
	return out.Bytes()
}

// The OOM guard: the byte cap bounds abuse, the pixel cap bounds memory —
// a header declaring >60 MP is refused on the DecodeConfig pass, before
// image.Decode can spend the heap.
func TestProcessRejectsOversizedPixels(t *testing.T) {
	_, err := Process(hugePNG(20000, 20000)) // 400 MP declared, 40 bytes real
	if err == nil {
		t.Fatal("a 400 MP declaration was accepted — the decode spike is unguarded")
	}
	for _, want := range []string{"20000×20000", "400 MP", "the cap is 60 MP"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must carry %q", err, want)
		}
	}
	// the boundary itself: a normal image sails through unchanged
	if _, err := Process(jpegBytes(320, 200)); err != nil {
		t.Fatalf("normal image rejected by the guard: %v", err)
	}
}

// MaxLabel is the handler's copy source: the 413 text must DERIVE from the
// constant, so bumping MaxBytes moves the message without a code edit.
func TestMaxLabel(t *testing.T) {
	if got := MaxLabel(); got != "100 MB" {
		t.Fatalf("MaxLabel() = %q, want %q", got, "100 MB")
	}
}

// The EXIF-strip promise: a JPEG whose APP1/EXIF segment is injected before
// the first frame must come out REBUILT — the encoder writes what it knows
// (pixels), not what arrived (metadata). We assert the artifact no longer
// carries the marker, which is the doctrine the §5 probe exists to enforce.
func TestProcessStripsMetadataByRebuild(t *testing.T) {
	raw := jpegBytes(64, 64)
	// splice a fake APP1 EXIF block after the SOI marker (FFD8)
	marker := []byte{0xFF, 0xE1, 0x00, 0x10, 'E', 'x', 'i', 'f', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	spliced := append([]byte{}, raw[:2]...)
	spliced = append(spliced, marker...)
	spliced = append(spliced, raw[2:]...)
	p, err := Process(spliced)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(p.Data, []byte("Exif")) {
		t.Fatal("EXIF survived the rebuild — the strip promise is broken")
	}
	if !bytes.Contains(spliced, []byte("Exif")) {
		t.Fatal("fixture lost its marker before the test even ran")
	}
}

// ---- role split: LUMEN_MEDIA_PREFIX (2026-11-13) ----

func TestWithPrefix(t *testing.T) {
	const k = "media/ab/abcdef.jpg"
	cases := []struct{ prefix, want string }{
		{"", k}, // dev default: byte-identical to the old shape
		{"staging", "staging/" + k},
		{"staging/", "staging/" + k}, // trailing slash normalized (no "//" keys)
		{"/cdn/x", "cdn/x/" + k},     // leading slash normalized likewise
		{"tenant/a/b", "tenant/a/b/" + k},
	}
	for _, c := range cases {
		if got := WithPrefix(c.prefix, k); got != c.want {
			t.Fatalf("WithPrefix(%q, %q) = %q want %q", c.prefix, k, got, c.want)
		}
	}
}

func TestURLPrefixCoherence(t *testing.T) {
	// the invariant: base + "/" + prefixed-key — the URL the portal embeds
	// verbatim must address EXACTLY the object key the relay puts
	key := WithPrefix("plane", "media/ab/sha.jpg")
	if got := URL("https://cdn.example.com", "", "", key); got != "https://cdn.example.com/plane/media/ab/sha.jpg" {
		t.Fatalf("cdn composition: %s", got)
	}
	if got := URL("", "http://minio:9000", "bucket", key); got != "http://minio:9000/bucket/plane/media/ab/sha.jpg" {
		t.Fatalf("dev composition: %s", got)
	}
	if got := URL("", "http://minio:9000/", "bucket", WithPrefix("", "media/x.jpg")); got != "http://minio:9000/bucket/media/x.jpg" {
		t.Fatalf("empty prefix must equal the pre-split URL: %s", got)
	}
}
