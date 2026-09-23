// thumbnail.go 生成媒体生成结果的缩略图：短边 ThumbShortEdge 像素的 JPEG。列表与任务
// 信息只看缩略图，原图 / 原视频只在放大查看或下载时才取回，一份几 MB 的 PNG 或
// 几十 MB 的视频不会为了画一行列表被整份拉到浏览器。
//
// 固件是不带 cgo 的静态二进制，只有标准库的 PNG / JPEG / GIF 解码：图像结果在落盘时
// 由设备生成缩略图；视频（H.264）与 WebP 等设备解不开的格式由页面在浏览器里抓一帧
// 回传（SaveThumb），设备照样按同一套规则解码、缩放、重新编码后保存——回传的字节
// 不原样落盘。
package mediagen

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
)

// ThumbShortEdge 是缩略图短边的目标像素数；源图短边不足时不放大。
const ThumbShortEdge = 480

// thumbJPEGQuality 是缩略图的 JPEG 质量：480 短边下 85 已看不出压缩痕迹，体积几十 KB。
const thumbJPEGQuality = 85

// maxThumbSourcePixels 是愿意解码的源图像素上限（解压炸弹防线）：平台图像最大几千
// 万像素，64 MP 以内解出的 RGBA 不超过 256 MiB。
const maxThumbSourcePixels = 64 << 20

// maxThumbUploadBytes 是页面回传封面帧的体积上限：一帧 480 短边的 JPEG 几十 KB，
// 页面偶尔按原尺寸回传也不过几 MB。
const maxThumbUploadBytes = 8 << 20

// ThumbExt 是缩略图文件的后缀：<任务 ID>.thumb.jpg，与结果文件同目录。
const ThumbExt = ".thumb.jpg"

// errThumbUndecodable：设备解不开这份图像（格式不在标准库之列或字节损坏）。
var errThumbUndecodable = errors.New("缩略图源图像无法解码")

func init() {
	// 显式注册三种标准库格式（image.Decode 按魔数认）；jpeg / png / gif 的 import
	// 已带注册副作用，这里只是把依赖写明。
	_ = png.Decode
	_ = jpeg.Decode
	_ = gif.Decode
}

// makeThumb 把 r 里的图像解码、缩到短边 ThumbShortEdge、编成 JPEG 字节。
func makeThumb(r io.Reader) ([]byte, error) { return MakeThumb(r, ThumbShortEdge) }

// MakeThumb 把 r 里的图像解码、缩到短边 shortEdge 像素（源图更小时不放大）、编成 JPEG
// 字节；解不开（格式不在标准库之列或字节损坏）返回错误。创作工作空间（internal/studio）
// 给目录里的图像做缩略图时用它。
func MakeThumb(r io.Reader, shortEdge int) ([]byte, error) {
	return MakeJPEG(r, shortEdge, thumbJPEGQuality)
}

// MakeJPEG 是 MakeThumb 带 JPEG 质量的形态：交给模型看的图像会随会话历史反复重发，
// 调用方按用途取更低的质量。quality 不在 1–100 时按缩略图质量。
func MakeJPEG(r io.Reader, shortEdge, quality int) ([]byte, error) {
	src, err := decodeBounded(r)
	if err != nil {
		return nil, err
	}
	if shortEdge <= 0 {
		shortEdge = ThumbShortEdge
	}
	if quality < 1 || quality > 100 {
		quality = thumbJPEGQuality
	}
	dst := scaleToShortEdge(src, shortEdge)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeBounded 先读图像头核对尺寸再解码：超过 maxThumbSourcePixels 的不解。
func decodeBounded(r io.Reader) (image.Image, error) {
	var head bytes.Buffer
	cfg, _, err := image.DecodeConfig(io.TeeReader(r, &head))
	if err != nil {
		return nil, errThumbUndecodable
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxThumbSourcePixels {
		return nil, fmt.Errorf("图像尺寸 %d×%d 超出缩略图处理范围", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(io.MultiReader(&head, r))
	if err != nil {
		return nil, errThumbUndecodable
	}
	return img, nil
}

// scaleToShortEdge 把 src 缩到短边 edge 像素（长边按比例，至少 1 像素），短边已不
// 大于 edge 时只做色彩规整。缩放用面积平均（box filter）：缩小场景下没有锯齿也
// 没有振铃，纯整数索引，不依赖 x/image。透明像素合成到白底——JPEG 没有 alpha。
func scaleToShortEdge(src image.Image, edge int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	flat := flatten(src)
	short := min(sw, sh)
	if short <= edge {
		return flat
	}
	var tw, th int
	if sw <= sh {
		tw, th = edge, max(1, int(int64(sh)*int64(edge)/int64(sw)))
	} else {
		th, tw = edge, max(1, int(int64(sw)*int64(edge)/int64(sh)))
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		y0, y1 := y*sh/th, (y+1)*sh/th
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < tw; x++ {
			x0, x1 := x*sw/tw, (x+1)*sw/tw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl uint64
			for sy := y0; sy < y1; sy++ {
				row := flat.Pix[sy*flat.Stride+x0*4 : sy*flat.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					r += uint64(row[i])
					g += uint64(row[i+1])
					bl += uint64(row[i+2])
				}
			}
			n := uint64((y1 - y0) * (x1 - x0))
			o := y*dst.Stride + x*4
			dst.Pix[o] = uint8(r / n)
			dst.Pix[o+1] = uint8(g / n)
			dst.Pix[o+2] = uint8(bl / n)
			dst.Pix[o+3] = 0xff
		}
	}
	return dst
}

// flatten 把任意 image.Image 合成到白底的 RGBA（原点归零）。
func flatten(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}
