# Oturumlar ve Konuşma Yönetimi

Microsoft 365 Copilot, konuşma geçmişini bulutta her konuşmaya özel bir `ConversationId` altında saklar. İstemcilerin her adımda tüm konuşma geçmişini tekrar tekrar sunucuya yüklemesine gerek kalmaması için M365Bridge, istemci oturum kimlikleri (Session ID) ile buluttaki M365 konuşmaları arasında kalıcı bir eşleşme kurar.

---

## Oturum Çözümleme Sırası

Her sohbet veya tamamlama isteğinde M365Bridge, geçerli oturum kimliğini şu öncelik sırasına göre belirler:

1. **Model Adı Soneki**: `model:sessionID` (örn. `gpt5.5-reasoning:proje-alfa`).
2. **`previous_response_id`**: `/v1/responses` istek gövdesinde yer alan kimlik.
3. **`session_id`**: İstek JSON gövdesindeki alan.
4. **`user`**: İstek JSON gövdesindeki kullanıcı kimliği alanı.
5. **`X-Session-Id`**: İstek başlığı.
6. **Ajan Özel Başlıkları**:
   - `X-Claude-Code-Session-Id` (Claude Code tarafından otomatik gönderilir).
   - `session-id` (OpenAI Codex CLI tarafından otomatik gönderilir).
7. **Belirleyici Karma (Hash) Yedeği**:
   - Kimlik doğrulama açıksa: `hash(api_key + ilk kullanıcı mesajı)`.
   - Kimlik doğrulama kapalıysa: `hash(ilk kullanıcı mesajı)`.

Bu öncelik sırası; Claude Code ve Codex gibi otonom araçların hiçbir ek ayar gerektirmeden doğrudan kendilerine ait izole bir konuşmada çalışmasını sağlarken, geliştiricilere oturumları başlık veya model adıyla yönetme esnekliği sunar.

---

## Model Adı Üzerinden Oturum Belirleme

İstemciler, model adının sonuna iki nokta (`:`) ve oturum adı ekleyerek konuşmayı belirli bir oturuma bağlayabilir:

```json
{
  "model": "gpt5.5-reasoning:frontend-inceleme",
  "messages": [{"role": "user", "content": "Kod incelemesine devam edelim"}]
}
```

Köprü, model adı olarak `gpt5.5-reasoning` seçeneğini alır ve konuşmayı doğrudan `frontend-inceleme` oturumuna yönlendirir.

---

## Sistem Yönergeleri ve Geliştirici Rolü

M365 Copilot geçmiş konuşmaları bulutta tuttuğu ve WebSocket üzerinden yalnızca son adımı aldığı için, önceki turlarda gönderilen sistem talimatları açıkça iletilmezse kaybolabilir.

- **Ön Ek Stratejisi**: İstekteki tüm `system` ve `developer` rolü mesajları birleştirilerek kullanıcının en son mesajının başına temiz bir ön ek olarak eklenir.
- **Anthropic Uyumluluğu**: En üst düzeydeki `system` dizgisi veya metin blokları da aynı şekilde birleştirilir.
- **Kayıt Temizliği**: Sistem talimatları konuşma geçmişi dosyalarına yazılmaz; böylece konuşma dökümleri temiz kalır.

---

## Oturum Yönetimi Endpoint'leri

M365Bridge, oturum eşleşmelerini denetlemek ve yönetmek için yönetim endpoint'leri sunar:

### 1. Oturumları Listeleme (`GET /v1/sessions`)
Yerel önbellekte (`data/cache/`) bulunan tüm aktif oturum eşleşmelerini döner:
```json
{
  "sessions": [
    {
      "session_id": "proje-alfa",
      "conversation_id": "a1b2c3d4-...",
      "last_updated": 1729051200
    }
  ]
}
```

### 2. Oturum Bilgisini İnceleme (`GET /v1/sessions/{id}`)
Belirli bir oturumun ilişkili olduğu M365 konuşma kimliğini ve güncellenme zamanını gösterir.

### 3. Oturumu Başka Bir Konuşmaya Bağlama (`PUT /v1/sessions/{id}`)
Yerel bir oturum kimliğini Microsoft bulutundaki mevcut bir konuşmaya bağlar:
```bash
curl -X PUT http://localhost:8230/v1/sessions/benim-oturum \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer api-anahtariniz" \
  -d '{"conversation_id": "hedef-m365-konusma-uuid"}'
```

### 4. Oturumu Silme (`DELETE /v1/sessions/{id}`)
Konuşmayı Microsoft bulut sunucularından siler ve yerel oturum eşleşmesini temizler:
- Buluttaki konuşmayı silmeden yalnızca yerel eşleşmeyi temizlemek için isteğin sonuna `?local_only=true` parametresi eklenebilir.
