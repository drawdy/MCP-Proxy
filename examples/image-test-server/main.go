package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:3007", "HTTP listen address")
	flag.Parse()

	server := newServer()
	handler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("MCP image test server listening at http://%s/sse", *addr)
		serverErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown error: %v", err)
			_ = httpServer.Close()
		}
		if err := <-serverErr; err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}
}

func newServer() *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "mcp-image-test-server", Version: "0.1.0"},
		nil,
	)

	server.AddTool(&mcp.Tool{
		Name:        "image_gallery",
		Description: "Returns mixed text, PNG and JPEG content for MCP image-output acceptance testing.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		pngData, err := makePNG()
		if err != nil {
			return nil, err
		}
		jpegData, err := makeJPEG()
		if err != nil {
			return nil, err
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Image gallery acceptance test. The first preview should be a landscape PNG."},
				&mcp.ImageContent{Data: pngData, MIMEType: "image/png"},
				&mcp.TextContent{Text: "The second preview should be a portrait JPEG. Text before and after images must retain this order."},
				&mcp.ImageContent{Data: jpegData, MIMEType: "image/jpeg"},
				&mcp.TextContent{Text: "End of image gallery test."},
			},
		}, nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "text_only",
		Description: "Returns a text-only result to verify that existing text rendering remains unchanged.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Text-only MCP result: rendering and Insert should continue to work."},
			},
		}, nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "svg_notice",
		Description: "Returns an SVG image block; the extension should show a safe unsupported-image notice.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="160" height="90"><rect width="160" height="90" fill="#f80"/></svg>`)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "The following SVG is intentionally unsupported by the extension preview."},
				&mcp.ImageContent{Data: svg, MIMEType: "image/svg+xml"},
			},
		}, nil
	})

	return server
}

func makePNG() ([]byte, error) {
	const width, height = 640, 360
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / (width - 1)),
				G: uint8(y * 255 / (height - 1)),
				B: 96,
				A: 255,
			})
		}
	}

	draw.Draw(img, image.Rect(36, 36, 604, 324), &image.Uniform{C: color.RGBA{R: 16, G: 24, B: 40, A: 100}}, image.Point{}, draw.Over)
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func makeJPEG() ([]byte, error) {
	const width, height = 360, 520
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	cx, cy := width/2, height/2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy < 115*115 {
				img.SetRGBA(x, y, color.RGBA{R: 245, G: 185, B: 55, A: 255})
				continue
			}
			angle := math.Atan2(float64(dy), float64(dx))
			r := uint8(35 + 70*(1+math.Sin(angle*3))/2)
			g := uint8(70 + 90*float64(y)/height)
			b := uint8(120 + 80*float64(x)/width)
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}

	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
