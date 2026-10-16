# Mimari ve Güvenlik

M365Bridge, Microsoft 365 Copilot'un özel SignalR WebSocket protokolü ile modern HTTP API standartları (OpenAI ve Anthropic) arasında yüksek performanslı bir köprü kurar.

---

## Veri Akış Mimarisi

```text
OpenAI / Anthropic İstemcisi (Claude Code, Cursor, SDK'lar)
  │
  ▼
pkg/servers (HTTP Adaptörü ve Protokol Dönüştürücüler)
  ├── Oturum Yönetimi, Durdurma Dizgileri, Jeton Tahmini
  └── Benzeştirilmiş Araç Çağırma ve Dahili Kodlama Araçları
  │
  ▼
pkg/payload (M365 JSON Yükü ve Ek Açıklamaların Hazırlanması)
  │
  ▼
pkg/client (İstek Başına TLS Üzerinden SignalR WebSocket)
  │
  ▼
wss://substrate.office.com/m365Copilot/Chathub
  │
  ▼
pkg/client StreamChunk Olay Döngüsü
  ├── Yanıt Biriktirici (İmleç yazımı ve anlık görüntü tekilleştirme)
  ├── Görsel referanslarının yerel rotalara dönüştürülmesi
  └── Düşünme (thinking) bloklarının ayrıştırılması
  │
  ▼
pkg/servers Streaming SSE / JSON Yanıtı
```

### Upstream Protokolü (SignalR)
- **İstek Başına Bağlantı**: M365 Copilot, ASCII kayıt ayırıcı (`\x1e`) karakterleriyle çerçevelenen SignalR WebSocket protokolünü kullanır.
- **Bağlantı İzolasyonu**: M365Bridge her istek için sıfırdan izole bir WebSocket bağlantısı açar; yanıt tamamlandığında veya istemci isteği iptal ettiğinde bağlantıyı güvenle kapatır.
- **Yanıt Biriktirici (Answer Accumulator)**: Copilot, yanıt metnini iki farklı kanal üzerinden gönderir: anlık imleç eklemeleri (`writeAtCursor`) ve tüm metnin yeniden gönderildiği anlık görüntüler (`messages[].text`). M365Bridge, geliştirdiği delta algoritmasıyla metin tekrarlarını ve ham alıntı kodlarını (`\ue200cite`) ayıklayarak temiz yanıt üretir.

---

## Güvenlik Modeli

### Kriptografi ve Kimlik Bilgisi Depolama
- **AES-256-GCM**: `data/tokens/` altında tutulan tüm hassas jetonlar ve çerezler AES-256-GCM ile diskte şifreli olarak saklanır.
- **Anahtar Yönetimi**: Ana şifreleme anahtarı ilk çalıştırmada otomatik üretilir ve `data/tokens/encryption.key` dosyasına katı dosya izinleriyle (`0600`) yazılır.
- **Minimum Disk İzi**: Konuşma geçmişi Microsoft bulutunda tutulur; yerel dökümler yalnızca `M365_ENABLE_WEB_UI=1` açıkken ve boyut sınırlarına tabi olarak diske yazılır.

### Atomik Dosya İşlemleri
Tüm durum dosyaları (yapılandırma, jeton önbellekleri, oturum kayıtları) `atomicfile.Write` mekanizmasıyla güncellenir:
1. Veri hedef dizinde geçici bir dosyaya yazılır.
2. Fiziksel diske senkronize edilir (`fsync`).
3. Geçici dosya hedef dosyanın üzerine atomik olarak yeniden adlandırılır (rename).
4. Üst dizin fiziksel diske senkronize edilir.
Bu yaklaşım; elektrik kesintisi veya ani kapanmalarda yarım yazılmış, bozuk kimlik dosyası kalmasını tamamen engeller.

### Sabit Zamanlı Karşılaştırmalar (Constant-Time Checks)
API anahtarı ve web arayüzü parolası kontrollerinde bayt tabanlı zamanlama saldırılarını (timing attacks) önlemek için `crypto/subtle.ConstantTimeCompare` fonksiyonu kullanılır.

---

## Proje Dizin Yapısı

```
cmd/cli/main.go            # Uygulama giriş noktası ve alt komut yönlendiricisi
pkg/
  atomicfile/              # Güvenli atomik dosya yazma işlemleri
  auth/auth.go             # TokenManager, OAuth 2.0 Device Code akışı, önbellek
  auth/sso.go              # Microsoft Designer için arka plan aracı jeton akışı
  client/client.go         # M365Client, SignalR WebSocket istemcisi
  client/conversations.go  # Bulut konuşmalarını listeleme, adlandırma, silme
  client/history.go        # M365 web sayfasından geçmiş konuşmaları okuma
  client/citations.go      # Yanıt içi kaynak linki dönüştürücüsü
  client/errors.go         # UpstreamError hata sınıflandırma mantığı
  codingtools/             # İsteğe bağlı yerel kodlama araçları yöneticisi
  crypto/crypto.go         # AES-256-GCM doğrulanmış şifreleme işlemleri
  logging/                 # Merkezi kayıtçı (logger)
  models/models.go         # Model kayıt defteri, yapılandırma, sürüm yönetimi
  payload/payload.go       # Copilot yük oluşturucuları, saat dilimi ve ekler
  servers/
    api.go                 # Ana HTTP sunucusu: OpenAI & Anthropic rotaları, SSE
    auth.go                # Web arayüzü parola denetimi ve açık rotalar
    cli.go                 # Etkileşimli CLI sunucusu
    errors.go              # Standart OpenAI hata formatı üreticisi
    mcp.go                 # Model Context Protocol JSON-RPC 2.0 sunucusu
    sessions.go            # Oturum - konuşma eşleştirme API'si
    stopsequence.go        # Akış esnasında durdurma dizgisi kesicisi
    transcripts.go         # Yerel konuşma geçmişi yöneticisi
    webui.go               # Gömülü web arayüzü HTTP dosya sunucusu
  setup/login.go           # RFC 8628 Cihaz Kodu ile giriş ve PKCE yedeği
  textcut/                 # Karakter (rune) güvenli metin kesme yardımcıları
  toolcalling/             # Benzeştirilmiş araç çağırma motoru ve ayrıştırıcı
  webui/embed.go           # Derlenmiş web arayüzünü Go ikilisine gömen direktif
web/                       # React 19 + TypeScript + Vite ön yüz projesi
docs/                      # Kılavuzlar ve ekran görüntüleri
data/                      # Çalışma zamanı verileri (gitignored): tokens/, transcripts/
```

---

## Bağımlılıklar

M365Bridge harici bir tarayıcıya (Chromium vb.) veya CGO derleyicisine ihtiyaç duymayan son derece hafif bir yapıya sahiptir:

| Bağımlılık                      | Kullanım Amacı                     |
|---------------------------------|------------------------------------|
| `github.com/google/uuid`        | Oturum ve istek kimlikleri üretimi |
| `github.com/gorilla/websocket`  | SignalR WebSocket istemcisi        |
| `github.com/pkoukk/tiktoken-go` | Yerel BPE jeton sayımı             |
| `github.com/dlclark/regexp2`    | Jetonlayıcı için düzenli ifadeler  |
