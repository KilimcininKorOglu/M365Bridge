# API Dokümantasyonu ve Referansı

M365Bridge; OpenAI ve Anthropic protokolleriyle tam HTTP uyumluluğu, gelişmiş oturum yönetimi, kota takibi, Copilot hesap belleği yönetimi ve Model Context Protocol (MCP) desteği sunar.

---

## Tüm Endpoint'ler Listesi

| Endpoint                          | Metot  | Protokol / Tür             | Kimlik Doğrulama | Açıklama                                                   |
|-----------------------------------|--------|----------------------------|------------------|------------------------------------------------------------|
| `/v1/chat/completions`            | POST   | OpenAI                     | Evet             | Sohbet tamamlama (streaming SSE ve tek parça yanıt)        |
| `/v1/completions`                 | POST   | OpenAI                     | Evet             | Klasik metin tamamlama endpoint'i                          |
| `/v1/responses`                   | POST   | OpenAI                     | Evet             | Responses API (Codex CLI ve modern ajanlar için)           |
| `/v1/responses/compact`           | POST   | OpenAI                     | Evet             | Uzak bağlam sıkıştırma (Context Compaction) endpoint'i     |
| `/v1/messages`                    | POST   | Anthropic                  | Evet             | Yerel Anthropic SSE formatıyla Messages API                |
| `/v1/messages/count_tokens`       | POST   | Anthropic                  | Evet             | Anthropic girdi jeton sayacı                               |
| `/v1/complete`                    | POST   | Anthropic                  | Evet             | Klasik Anthropic tamamlama endpoint'i                      |
| `/v1/models`                      | GET    | Ortak (OpenAI & Anthropic) | Hayır            | Her iki protokolün şemasına da uyan birleşik model listesi |
| `/v1/quota`                       | GET    | M365Bridge                 | Evet             | Son gözlemlenen M365 konuşma mesajı kotası                 |
| `/v1/personalization`             | GET    | M365Bridge                 | Evet             | Hesap düzeyindeki Copilot bellek ve kişiselleştirme durumu |
| `/v1/personalization`             | PATCH  | M365Bridge                 | Evet             | Hesap düzeyindeki Copilot belleğini açma/kapatma           |
| `/v1/sessions`                    | GET    | M365Bridge                 | Evet             | Aktif oturum - konuşma eşleşmelerini listeler              |
| `/v1/sessions/{id}`               | GET    | M365Bridge                 | Evet             | Belirli bir oturumun M365 konuşma kimliğini döner          |
| `/v1/sessions/{id}`               | PUT    | M365Bridge                 | Evet             | Bir oturumu mevcut bir M365 konuşmasına bağlar             |
| `/v1/sessions/{id}`               | DELETE | M365Bridge                 | Evet             | Buluttaki konuşmayı siler ve yerel oturum kaydını temizler |
| `/v1/sessions/{id}/messages`      | GET    | M365Bridge                 | Evet             | Oturuma ait yerel kayıt dosyasındaki mesajları döner       |
| `/v1/conversations`               | GET    | M365 Web                   | Evet             | M365 hesabındaki bulut konuşmalarını listeler              |
| `/v1/conversations`               | POST   | M365 Web                   | Evet             | İlk mesajla yeni bir konuşma başlatır                      |
| `/v1/conversations/{id}`          | PATCH  | M365 Web                   | Evet             | Konuşmayı yeniden adlandırır (`{"name": "..."}`)           |
| `/v1/conversations/{id}`          | DELETE | M365 Web                   | Evet             | Buluttaki konuşmayı ve bağlı oturumları siler              |
| `/v1/conversations/{id}/messages` | GET    | M365 Web                   | Evet             | M365 web sayfasından tüm konuşma geçmişini çeker           |
| `/v1/images/generations`          | POST   | OpenAI                     | Evet             | Microsoft Designer ile metinden görsel üretir              |
| `/v1/images/edits`                | POST   | OpenAI                     | Evet             | Mevcut görselleri düzenler                                 |
| `/v1/images/{ref}`                | GET    | M365Bridge                 | Evet             | Üretilen bir görseli köprü üzerinden indirir               |
| `/mcp`                            | POST   | JSON-RPC 2.0               | Evet             | Model Context Protocol sunucu endpoint'i                   |
| `/health`                         | GET    | Sistem                     | Hayır            | Sağlık kontrolü (Liveness probe)                           |
| `/v1/health`                      | GET    | Sistem                     | Hayır            | Codex CLI erişilebilirlik kontrolü                         |
| `/v1/auth`                        | GET    | Sistem                     | Hayır            | Web arayüzü giriş kapısı durumunu bildirir                 |
| `/v1/auth/verify`                 | POST   | Sistem                     | Hayır            | Sunulan kimlik bilgisini doğrular                          |
| `/`                               | GET    | Web Arayüzü                | Hayır*           | Web arayüzü (varsa parola sorar)                           |

---

## Çift Formatlı Model Kayıt Defteri (`GET /v1/models`)

Hem OpenAI hem de Anthropic istemcileri modelleri `GET /v1/models` üzerinden keşfeder. Yanıt gövdesi her iki standardı aynı anda karşılayacak şekilde hazırlanmıştır:
- OpenAI alanları: `object: "model"`, `id`, `created`, `owned_by`.
- Anthropic alanları: `type: "model"`, `display_name`, `created_at`, `max_tokens`, `capabilities`.
- Sayfalama: `has_more: false`, `first_id` ve `last_id`.
Claude Code bu rotayı okur, `claude` veya `anthropic` ile başlayan model kimliklerini filtreler ve yetenek ağacını buna göre yapılandırır.

---

## Responses API ve Sıkıştırma (Compact API)

### Düşünme Yoğunluğu (Reasoning Effort)
`/v1/responses` rotası `reasoning: {"effort": "<seviye>"}` parametresini destekler:
- Kabul edilen değerler: `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`.
- `medium` veya daha yüksek seviyeler, isteği otomatik olarak modelin reasoning (düşünme) varyantına yönlendirir (örneğin `gpt5.5` -> `gpt5.5-reasoning`).

### Streaming Olay Akışı
`/v1/responses` üzerindeki streaming istekleri şu SSE olaylarını üretir:
`response.created` -> `response.in_progress` -> `response.output_item.added` -> `response.output_text.delta` -> `response.output_text.done` -> `response.completed`.

### Bağlam Sıkıştırma (`/v1/responses/compact`)
Codex CLI gibi otonom ajanların uzun bağlam pencerelerini özetleyip sıkıştırması için kullanılır:
1. Konuşma geçmişi tek bir özetleme istemi haline getirilir.
2. Copilot arka planında bu geçmiş özetlenir.
3. Yanıt `encrypted_content` içeren yapılandırılmış bir `compaction` öğesi olarak döner.

---

## Jeton Sayımı ve Kullanım Raporlama (Token Usage)

M365 SignalR arka ucu jeton kullanım verisi döndürmediğinden, M365Bridge bu değerleri yerel olarak hesaplar:
- **Jetonlayıcı (Tokenizer)**: GPT-5 sınıfı modellere uygun `o200k_base` BPE jetonlayıcısı kullanılır (yedek olarak `cl100k_base`).
- **Düşünme Jetonları**: `reasoning_tokens` alanında ayrıca bildirilir.
- **Kaynak Metaverisi**: Her yanıtta `usage_source: "tiktoken_o200k_base_estimate"` bilgisi yer alır.

Örnek OpenAI kullanım nesnesi:
```json
{
  "prompt_tokens": 42,
  "completion_tokens": 17,
  "reasoning_tokens": 6,
  "total_tokens": 59,
  "usage_source": "tiktoken_o200k_base_estimate"
}
```

---

## Durdurma Dizgileri (Stop Sequences)

Tüm tamamlama uçlarında desteklenir (OpenAI için `stop`, Anthropic için `stop_sequences`):
- **Akış Esnasında Anında Kesme**: Özel `stopSequenceWriter` mekanizması, gelen akışı karakter sınırlarında denetler ve belirtilen durdurma dizgisi yakalandığı anda yayını keser.
- Eşleşen durdurma dizgisi çıktı metnine dahil edilmez.
- Anthropic tarafında `stop_reason: "stop_sequence"` döner ve eşleşen dizi adı bildirilir.

---

## Hata Yönetimi ve Hata Sınıflandırma

Tüm hatalar standart OpenAI JSON formatında sunulur:
```json
{
  "error": {
    "message": "Açıklayıcı hata mesajı",
    "type": "invalid_request_error | authentication_error | rate_limit_error | server_error",
    "code": "machine_readable_code"
  }
}
```

### Hata Kodları Tablosu
| HTTP Kodu | Hata Kodu (`code`)         | Nedeni                                                     |
|-----------|----------------------------|------------------------------------------------------------|
| `401`     | `upstream_auth_failed`     | Kimlik bilgileri geçersiz, süresi dolmuş veya yenilenemedi |
| `403`     | `insufficient_permissions` | Microsoft kurumsal ilkesi isteği reddetti                  |
| `429`     | `rate_limit_exceeded`      | İstek hız sınırına takıldı; `Retry-After` başlığı döner    |
| `429`     | `upstream_throttled`       | Konuşma mesaj kotası doldu                                 |
| `409`     | `tool_round_limit`         | İstemci `M365_MAX_TOOL_ROUNDS` tur sınırını aştı           |
| `404`     | `model_not_found`          | İstenen model `/v1/models` listesinde yok                  |
| `502`     | `upstream_error`           | Microsoft arka ucu isteği reddetti veya ulaşılamıyor       |
| `502`     | `upstream_unavailable`     | WebSocket el sıkışması başarısız oldu veya bağlantı koptu  |
| `502`     | `upstream_turn_failed`     | Arka uç yanıt üretmeden oturumu sonlandırdı                |
| `502`     | `upstream_content_blocked` | İçerik Microsoft güvenlik filtrelerine takıldı             |
| `504`     | `upstream_timeout`         | Arka uçtan beklenen süre içinde yanıt gelmedi              |

---

## Kota Takibi ve Hesap Belleği

### Konuşma Mesaj Kotası (`GET /v1/quota`)
Copilot, konuşma başına mesaj sayısı sınırı uygular (genellikle 600) ve bunu her adımda güncellenen akış çerçevelerinde bildirir. `GET /v1/quota` en son kaydedilen sayaçları döner:
```json
{
  "object": "quota",
  "available": true,
  "exhausted": false,
  "used": 8,
  "max": 600,
  "headroom": 592
}
```

### Copilot Kişiselleştirme ve Hesap Belleği (`/v1/personalization`)
Microsoft 365 Copilot, konuşmalar arası bir hesap belleği tutar. Konuşmaların birbirini etkilememesi için M365Bridge bu ayarı kontrol etmenize olanak tanır:
- `GET /v1/personalization`: Geçerli hesap belleği durumunu sorgular.
- `PATCH /v1/personalization` (`{"memory_enabled": false}`): Hesap düzeyindeki belleği kapatır.
