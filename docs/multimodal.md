# Multimodal & Image Generation

M365Bridge supports comprehensive multimodal workflows: image understanding across both OpenAI and Anthropic specifications, and AI image generation via Microsoft Designer.

---

## Image Input (Vision)

You can attach images to your requests using standard provider formats:

### Supported Formats
- **OpenAI Chat Completions**:
  ```json
  {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
  ```
- **Anthropic Messages**:
  ```json
  {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}
  ```
- **Responses API**:
  ```json
  {"type": "input_image", "image_url": "data:image/png;base64,..."}
  ```

Supported image mime types include **PNG**, **JPEG**, **GIF**, and **WebP**. Images are uploaded directly to M365 Copilot's secure storage and referenced within the SignalR message annotations.

### Remote Image URLs & SSRF Protection
OpenAI requests may provide remote `https://` URLs instead of inline base64 strings. M365Bridge downloads these images server-side before submitting them to Copilot.

To prevent Server-Side Request Forgery (SSRF):
- Plain HTTP targets are rejected; only `https://` is permitted.
- Hostnames resolving to loopback (`127.0.0.1`), RFC 1918 private subnets (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), link-local addresses, carrier-grade NAT, or cloud metadata endpoints (`169.254.169.254`) are strictly blocked.
- Individual image downloads are limited to 20 MB, with a maximum of 16 images per request.

---

## Image Generation (Microsoft Designer)

M365Bridge integrates with Microsoft Designer through two mechanisms:

### 1. In-Chat Generation
Asking for an image in ordinary chat endpoints (`/v1/chat/completions`, `/v1/messages`, `/v1/responses`) triggers Copilot's Designer capability. 

- **Progress Notice**: Because image generation takes roughly 45–60 seconds, streaming endpoints emit an SSE comment as soon as generation starts:
  ```
  : notice image_generating
  ```
  Standard OpenAI and Anthropic clients ignore SSE comments safely, while the built-in web UI displays an interactive status indicator.
- **Image Proxying**: Upstream image URLs require internal Microsoft tokens. M365Bridge converts these into gateway-relative paths:
  ```markdown
  ![Generated Image](/v1/images/2f1c8b7e-xxxx-xxxx-xxxx-xxxxxxxxxxxx)
  ```
  These internal references are valid for 12 hours and can be fetched directly using the client's API key.

### 2. Dedicated Images API (`/v1/images/*`)
M365Bridge provides OpenAI-compatible image endpoints:
- `POST /v1/images/generations`: Generate images from a text prompt.
- `POST /v1/images/edits`: Edit or iterate on existing images via multipart form uploads.

#### Supported Parameters
| Parameter         | Type    | Default     | Description                                                 |
|-------------------|---------|-------------|-------------------------------------------------------------|
| `prompt`          | string  | *Required*  | Text prompt describing the image                            |
| `n`               | integer | `1`         | Number of images to generate (M365 generates 1 per request) |
| `size`            | string  | `1024x1024` | Requested image dimensions                                  |
| `quality`         | string  | `standard`  | `standard` or `hd` hint appended to prompt                  |
| `style`           | string  | `natural`   | `natural` or `vivid` style hint                             |
| `response_format` | string  | `url`       | `url` (returns base64 data URI) or `b64_json`               |

#### Python Generation Example
```python
from openai import OpenAI
import base64

client = OpenAI(base_url="http://localhost:8230/v1", api_key="your-api-key")

resp = client.images.generate(
    model="gpt5.5-reasoning",
    prompt="A serene cyberpunk city covered in mist at twilight",
    n=1,
    response_format="b64_json",
)

with open("cyberpunk.png", "wb") as f:
    f.write(base64.b64decode(resp.data[0].b64_json))
```

### Security & Token Flow
Downloading generated images from Microsoft Designer uses a native OAuth Broker token exchange (`designerappservice.officeapps.live.com`). This runs 100% headlessly in the background without requiring browser instances or session cookies.

Host destinations are verified against `M365_IMAGE_HOST_ALLOWLIST` (defaulting to `.officeapps.live.com`).
