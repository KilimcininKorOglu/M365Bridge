# Çok Modlu (Multimodal) Yetenekler ve Görsel Üretimi

M365Bridge; hem OpenAI hem de Anthropic standartlarında görsel anlama (görsel girdi analizi) ve Microsoft Designer üzerinden yapay zeka ile görsel üretimi yeteneklerini destekler.

---

## Görsel Girdisi (Vision)

Standart sağlayıcı formatlarını kullanarak sohbet isteklerinize görsel ekleyebilirsiniz:

### Desteklenen Formatlar
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

Desteklenen görsel biçimleri: **PNG**, **JPEG**, **GIF** ve **WebP**. Yüklenen görseller güvenli bir şekilde Copilot depolama alanına aktarılır ve SignalR mesaj eklerinde referans olarak kullanılır.

### Uzak Görsel URL'leri ve SSRF Koruması
OpenAI isteklerinde base64 verisi yerine doğrudan harici bir `https://` görsel adresi verilebilir. M365Bridge bu görselleri Copilot'a iletmeden önce sunucu tarafında indirir.

Sunucu Taraflı İstek Sahteciliği (SSRF) saldırılarını engellemek için:
- Şifresiz HTTP bağlantıları reddedilir; yalnızca `https://` kabul edilir.
- Geri döngü (loopback: `127.0.0.1`), yerel ağ alt ağları (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), yerel bağlantı (link-local) ve bulut metaveri adresleri (`169.254.169.254`) kesin olarak engellenir.
- Görsel boyutu azami 20 MB ile, istek başına görsel sayısı ise 16 ile sınırlandırılmıştır.

---

## Görsel Üretimi (Microsoft Designer)

M365Bridge, Microsoft Designer entegrasyonunu iki farklı yöntemle sunar:

### 1. Sohbet Esnasında Doğrudan Üretim
Standart sohbet uçlarında (`/v1/chat/completions`, `/v1/messages`, `/v1/responses`) bir görsel üretilmesini istediğinizde Copilot'un yerleşik Designer yeteneği devreye girer.

- **İlerleme Bildirimi**: Görsel üretimi yaklaşık 45–60 saniye sürer. Bu esnada streaming uç noktaları istemciye anında şu SSE yorumunu gönderir:
  ```
  : notice image_generating
  ```
  Standart OpenAI ve Anthropic istemcileri bu SSE yorumunu göz ardı eder; yerleşik web arayüzü ise kullanıcıya şık bir "görsel üretiliyor" durumu gösterir.
- **Görsel Köprüleme (Proxying)**: Microsoft'un ürettiği orijinal görsel bağlantıları özel oturum jetonları gerektirir. M365Bridge bu bağlantıları yerel bir köprü adresine dönüştürür:
  ```markdown
  ![Üretilen Görsel](/v1/images/2f1c8b7e-xxxx-xxxx-xxxx-xxxxxxxxxxxx)
  ```
  Bu referans 12 saat boyunca geçerlidir ve istemcinin API anahtarıyla doğrudan indirilebilir.

### 2. Bağımsız Görsel API'si (`/v1/images/*`)
OpenAI ile uyumlu bağımsız görsel uç noktaları:
- `POST /v1/images/generations`: Metin isteminden görsel üretir.
- `POST /v1/images/edits`: Mevcut görselleri düzenler veya varyasyon üretir (multipart form formatında).

#### Desteklenen Parametreler
| Parametre         | Tip     | Varsayılan  | Açıklama                                                   |
|-------------------|---------|-------------|------------------------------------------------------------|
| `prompt`          | string  | *Zorunlu*   | Üretilecek görseli tarif eden metin                        |
| `n`               | integer | `1`         | Üretilecek görsel sayısı (M365 istek başına 1 adet üretir) |
| `size`            | string  | `1024x1024` | İstenen görsel boyutu                                      |
| `quality`         | string  | `standard`  | İstem sonuna eklenen kalite ipucu (`standard` veya `hd`)   |
| `style`           | string  | `natural`   | Görsel tarzı (`natural` veya `vivid`)                      |
| `response_format` | string  | `url`       | `url` (data URI döner) veya `b64_json`                     |

#### Python ile Görsel Üretim Örneği
```python
from openai import OpenAI
import base64

client = OpenAI(base_url="http://localhost:8230/v1", api_key="api-anahtariniz")

resp = client.images.generate(
    model="gpt5.5-reasoning",
    prompt="Gün batımında sisler içinde huzurlu bir siberpunk şehir manzarası",
    n=1,
    response_format="b64_json",
)

with open("cyberpunk.png", "wb") as f:
    f.write(base64.b64decode(resp.data[0].b64_json))
```

### Güvenlik ve Jeton Akışı
Microsoft Designer tarafından üretilen görsellerin indirilmesi, arka planda yerel bir OAuth Broker jeton değişimi (`designerappservice.officeapps.live.com`) ile gerçekleşir. Tarayıcı veya çerez gerektirmeyen bu süreç tamamen başsız (headless) çalışır.

Görsel indirme adresleri `M365_IMAGE_HOST_ALLOWLIST` (varsayılan `.officeapps.live.com`) listesindeki güvenilir sunucularla sınırlandırılmıştır.
