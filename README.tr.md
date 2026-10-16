# M365Bridge

[![CI](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml)
[![Release](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Version](https://img.shields.io/github/v/release/KilimcininKorOglu/M365Bridge)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://github.com/KilimcininKorOglu/M365Bridge/pkgs/container/m365bridge)
[![Go](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![OpenAI Uyumlu](https://img.shields.io/badge/API-OpenAI%20Compatible-412991)](#hızlı-kullanım)
[![Anthropic Uyumlu](https://img.shields.io/badge/API-Anthropic%20Compatible-D97757?logo=anthropic&logoColor=white)](#hızlı-kullanım)

**[English](README.md)** | **Türkçe**

M365Bridge, Microsoft 365 Copilot aboneliğinizi standart bir **OpenAI** ve **Anthropic** uyumlu HTTP API'ye dönüştürür. **Claude Code**, **Cursor**, **Codex**, **Cline**, standart Python/Node.js SDK'ları ve geliştirdiğiniz tüm yapay zeka araçlarını doğrudan Copilot altyapısıyla çalıştırabilirsiniz.

![M365Bridge Web Arayüzü](docs/webui-tr.png)

```text
Yapay Zeka Aracınız (Claude Code, Cursor, SDK)
  -> M365Bridge (:8230)
  -> substrate.office.com (SignalR WebSocket)
  -> M365 Copilot (GPT-5, GPT-6, Claude)
```

---

## Öne Çıkan Özellikler

- **Tarayıcısız Kolay Giriş (Device Code Flow)**: `m365-bridge login` komutuyla saniyeler içinde bağlanın. Sunucuda tarayıcı kurmanıza, yan konteyner (sidecar) çalıştırmanıza veya çerezlerle uğraşmanıza gerek kalmaz.
- **90 Günlük Otomatik Dönen Jeton**: Yerel genel istemci (RFC 8628) olarak kimlik doğrular. 24 saatlik web oturumu kısıtlamasına takılmaz; jetonlar arka planda otomatik yenilenir.
- **Eksiksiz OpenAI ve Anthropic Uyumluluğu**: Gerçek zamanlı akış (SSE), Responses API, bağlam sıkıştırma (compact) ve jeton sayım rotaları.
- **Benzeştirilmiş Araç Çağırma (Tool Calling)**: Geliştirici tanımlı fonksiyonları şema doğrulaması ve kısır döngü korumasıyla güvenle çalıştırır.
- **Görsel Analizi ve Üretimi**: İki protokolde de çok modlu (vision) görsel girdisi ve Microsoft Designer ile metinden görsel üretimi.
- **Düşünme Süreci (Reasoning)**: Modelin iç akıl yürütme adımlarını `reasoning_content` (OpenAI) ve `thinking` (Anthropic) bloklarıyla sunar.
- **Yerleşik Web Arayüzü**: Go ikili dosyasına gömülü, çok turlu sohbet ve markdown desteği sunan Türkçe/İngilizce arayüz.
- **Model Context Protocol (MCP)**: Copilot'u `/mcp` rotasında standart bir JSON-RPC 2.0 sunucusu olarak sunar.

---

## Hızlı Kurulum

### Seçenek A: Docker (Önerilen)

1. Bir `docker-compose.yml` dosyası oluşturun:
   ```yaml
   services:
     m365bridge:
       image: ghcr.io/kilimcininkoroglu/m365bridge:latest
       container_name: m365bridge
       environment:
         - TZ=Europe/Istanbul
       ports:
         - "8230:8000"
       volumes:
         - ./data:/app/data
       restart: unless-stopped
   ```
2. Konteyneri başlatın:
   ```bash
   docker compose up -d
   ```
   *(Alternatif olarak tek satırda `docker run`: `docker run -d --name m365bridge -p 8230:8000 -v "$(pwd)/data:/app/data" --restart unless-stopped ghcr.io/kilimcininkoroglu/m365bridge:latest`)*

3. Microsoft hesabınızla oturum açın:
   ```bash
   docker exec -it m365bridge ./bin/m365-bridge login
   ```

### Seçenek B: Hazır İkili Dosya (Pre-built Binary)

İşletim sisteminize uygun derlenmiş dosyayı [Sürümler (Releases)](https://github.com/KilimcininKorOglu/M365Bridge/releases) sayfasından indirin:

| İşletim Sistemi / Mimari          | Dosya Adı                       |
|-----------------------------------|---------------------------------|
| Linux x86_64                      | `m365-bridge-linux-amd64`       |
| Linux ARM64                       | `m365-bridge-linux-arm64`       |
| macOS Apple Silicon (M1/M2/M3/M4) | `m365-bridge-darwin-arm64`      |
| macOS Intel                       | `m365-bridge-darwin-amd64`      |
| Windows x86_64                    | `m365-bridge-windows-amd64.exe` |

```bash
mkdir m365bridge && cd m365bridge
curl -L -o m365-bridge https://github.com/KilimcininKorOglu/M365Bridge/releases/latest/download/m365-bridge-linux-amd64
chmod +x m365-bridge
mkdir data

# Giriş yapın
./m365-bridge login

# Sunucuyu başlatın
./m365-bridge serve --port 8000
```

### Seçenek C: Kaynaktan Derleme

```bash
git clone https://github.com/KilimcininKorOglu/M365Bridge.git
cd M365Bridge
go build -o bin/m365-bridge ./cmd/cli
mkdir -p data
./bin/m365-bridge login
./bin/m365-bridge serve --port 8000
```

---

## Hesabınızı Bağlama (Cihaz Kodu ile Giriş)

Microsoft 365 Copilot hesabınızı bağlamak 30 saniyeden az sürer:

```bash
# Docker kullanıyorsanız:
docker exec -it m365bridge ./bin/m365-bridge login

# İkili dosya kullanıyorsanız:
./m365-bridge login
```

1. Terminalde bir doğrulama adresi ve 8 haneli bir kod görüntülenir:
   ```text
   Herhangi bir cihazdan (telefon, tablet, bilgisayar) giriş yapmak için:
     1. Adresi açın: https://login.microsoft.com/device
     2. Kodu girin  : ABCD-1234
   ```
2. Dilediğiniz bir cihazdan adresi açın, kodu girin ve Microsoft hesabınızla onay verin.
3. Komut satırı onayı otomatik algılar, 90 günlük dinamik yenileme jetonunu alır, bilgileri AES-256-GCM ile şifreleyerek `data/tokens/` altına kaydeder ve `data/.env` dosyasını oluşturur.

> [!TIP]
> Kurumunuz cihaz kodu akışını kısıtlıyorsa, `./m365-bridge login -pkce` komutuyla tarayıcı yönlendirmeli PKCE girişini kullanabilirsiniz.

---

## Desteklenen Modeller

M365Bridge, model isimlerini arka plandaki ilgili Copilot tonlarına yönlendirir:

| Model Adı          | Copilot Tonu        | OpenAI Eşdeğer ID   | Düşünme (Thinking) | Arka Uç |
|--------------------|---------------------|---------------------|--------------------|---------|
| `auto`             | Magic               | `gpt-4-auto`        | Hayır              | GPT-5   |
| `quick`            | Chat                | `gpt-4-quick`       | Hayır              | GPT-5   |
| `gpt5.5`           | Gpt_5_5_Chat        | `gpt5.5`            | Hayır              | GPT-5   |
| `gpt5.5-reasoning` | Gpt_5_5_Reasoning   | `gpt5.5-reasoning`  | Evet               | GPT-5   |
| `gpt5.6`           | Gpt_5_6_Chat        | `gpt5.6`            | Hayır              | GPT-5   |
| `gpt5.6-reasoning` | Gpt_5_6_Reasoning   | `gpt5.6-reasoning`  | Evet               | GPT-5   |
| `gpt6sol`          | Gpt_6_Sol_Reasoning | `gpt-6-sol`         | Hayır              | GPT-6   |
| `gpt6-astra`       | GPT_6_Astra         | `gpt-6-astra`       | Hayır              | GPT-6   |
| `claude-sonnet`    | Claude_Sonnet       | `claude-sonnet-4.6` | Hayır              | Claude  |
| `claude-opus`      | Claude_Opus         | `claude-opus-4.6`   | Evet               | Claude  |

### Model Tavsiyeleri
| İhtiyaç                                            | Önerilen Model                     |
|----------------------------------------------------|------------------------------------|
| Genel sohbet ve hızlı sorular                      | `auto` veya `gpt5.5`               |
| Kodlama ajanları, araç çağırma ve karmaşık mantık  | `gpt5.5-reasoning`                 |
| Claude uyumlu araçlar (Claude Code, Anthropic SDK) | `claude-sonnet` veya `claude-opus` |
| En güncel akıl yürütme modeli                      | `gpt6sol`                          |

---

## Hızlı Kullanım

### 1. Komut Satırı Arayüzü (CLI)
```bash
# Tek bir soru sorup yanıt alma
./m365-bridge "Mutex ile semaphore arasındaki fark nedir?"

# Etkileşimli çok turlu sohbet oturumu
./m365-bridge -i --model gpt5.5-reasoning
```

### 2. cURL (HTTP API)
```bash
curl http://localhost:8230/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt5.5-reasoning",
    "messages": [{"role": "user", "content": "Merhaba!"}]
  }'
```

### 3. Python (OpenAI SDK)
```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8230/v1", api_key="gerekmez")

response = client.chat.completions.create(
    model="gpt5.5-reasoning",
    messages=[{"role": "user", "content": "Goroutine mantığı nasıl çalışır?"}],
)
print(response.choices[0].message.content)
```

### 4. Python (Anthropic SDK)
```python
from anthropic import Anthropic

client = Anthropic(base_url="http://localhost:8230", api_key="gerekmez")

message = client.messages.create(
    model="claude-opus",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Go ile hızlı sıralama (quicksort) yaz"}],
)
print(message.content[0].text)
```

---

## Kapsamlı Dokümantasyon

Tüm teknik detaylar için `docs/` dizinindeki kılavuzları inceleyebilirsiniz:

- **[Yapılandırma Kılavuzu](docs/configuration.tr.md)**: Ortam değişkenleri, web arayüzü parolası ve API anahtarı ayarları.
- **[API Dokümantasyonu](docs/api.tr.md)**: Endpoint listesi, OpenAI/Anthropic protokol detayları, kota takibi, hata formatları ve bağlam sıkıştırma.
- **[Araç Çağırma ve Kodlama Araçları](docs/tool-calling.tr.md)**: Fonksiyon çağırma mantığı, Claude Code/Codex döngü yönetimi ve sunucu tarafı kodlama araçları.
- **[Çok Modlu Yetenekler ve Görsel Üretimi](docs/multimodal.tr.md)**: Görsel girdi analizi, SSRF güvenlik filtreleri ve Microsoft Designer entegrasyonu.
- **[Oturumlar ve Konuşma Yönetimi](docs/sessions.tr.md)**: Bağlam sürekliliği, model sonekiyle oturum yönlendirme (`model:oturum`) ve bulut konuşma yönetimi.
- **[Mimari ve Güvenlik](docs/architecture.tr.md)**: SignalR veri akışı, AES-256-GCM şifreleme modeli, atomik dosya işlemleri ve proje yapısı.

---

## Yasal Uyarı ve Lisans

Bu proje yalnızca eğitim ve araştırma amaçlıdır. Herkese açık ağ iletişim protokollerini inceler; hiçbir güvenlik mekanizmasını kırmaz, hesap verilerini sızdırmaz ve Microsoft servislerine yetkisiz müdahale etmez. Microsoft Corporation ile resmi bir bağı yoktur.

Yalnızca araştırma ve kişisel kullanım lisansıyla sunulmaktadır (**Research Only**).
