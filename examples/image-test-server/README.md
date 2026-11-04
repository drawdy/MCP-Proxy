# MCP image-output manual test backend

This example starts a deterministic local MCP Server that returns mixed text/image/text results. It is intended for manual acceptance of MCP-Interceptor image previews and attachment behavior.

## Start the test backend

From the MCP-Proxy repository root:

```sh
go run ./examples/image-test-server
```

The backend listens on `http://127.0.0.1:3007/sse` and exposes:

- `image_gallery`: text + generated PNG + text + generated JPEG + text.
- `text_only`: verifies the existing text-only result path.
- `svg_notice`: returns SVG; the extension should reject preview and show an unsupported-image notice.

## Start MCP-Proxy

In a second terminal:

```sh
go run ./cmd/mcp-proxy --config examples/image-test-server/proxy-config.json
```

MCP-Proxy uses its default local endpoint: `http://127.0.0.1:3006/sse`.

## Connect MCP-Interceptor

1. Build the extension from the MCP-Interceptor repository with `pnpm build`.
2. Load the generated `dist` directory as an unpacked Chrome extension.
3. In the extension popup, add/connect an SSE endpoint: `http://127.0.0.1:3006/sse`.
4. Confirm the tool list contains `image-test.image_gallery`, `image-test.text_only` and `image-test.svg_notice`.
5. Run `image-test.image_gallery` from a ChatGPT/Gemini/DeepSeek/Grok conversation.
6. Verify the result panel displays the text and two images in their original order.
7. Click **Attach Images**. Verify both images appear in the composer and the text context is inserted once.
8. Verify the extension does not auto-submit the conversation.
9. Run `image-test.svg_notice` and confirm no SVG preview is created; a safe unsupported-image notice should be shown.
10. Run `image-test.text_only` and confirm the existing text-only Insert flow still works.

Keep both local services bound to loopback. This test backend has no authentication and must not be exposed to the public internet.

## Automated validation

Run `go test ./examples/image-test-server` to check all three tools through MCP-Proxy over SSE, including image decoding, dimensions, and content order. Browser preview, composer attachment, and Insert behavior still require the manual checks above.
