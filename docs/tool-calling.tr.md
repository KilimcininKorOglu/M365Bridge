# Araç Çağırma (Tool Calling) ve Kodlama Araçları

Microsoft 365 Copilot, kullanıcı tarafından tanımlanan işlevleri yerel olarak çalıştırmaz. M365Bridge, geliştirdiği **Benzeştirilmiş Araç Çağırma (Simulated Tool Calling)** katmanı sayesinde bu boşluğu doldurur. Böylece **Claude Code**, **Codex**, **Cursor** ve kendi geliştirdiğiniz otonom ajanlar işlev tanımlayıp sorunsuzca çalıştırabilir.

---

## Araç Çağırma Mimarisi

1. **İstek Dönüştürme**: İstemcinin gönderdiği `tools` dizisi (OpenAI veya Anthropic formatında) arka planda Copilot'un anlayacağı yapılandırılmış bir istem haline getirilir.
2. **Model Çağrısı**: Copilot, araç çağrısını markdown blokları içinde yapılandırılmış bir JSON olarak üretir.
3. **Ayrıştırma ve Doğrulama**: M365Bridge bu çıktıyı ayrıştırır, parametreleri JSON Şeması kurallarına göre doğrular ve OpenAI `tool_calls` veya Anthropic `tool_use` bloklarına dönüştürür.
4. **Çalıştırma**: İstemci uygulaması ilgili aracı kendi ortamında çalıştırır ve sonucunu bir sonraki konuşma adımında geri gönderir.

Bu mekanizma `/v1/chat/completions`, `/v1/messages` ve `/v1/responses` rotalarında hem streaming hem de tek parça yanıt modunda eksiksiz çalışır.

---

## Araç Doğrulama ve Sözleşme Kuralları

- **Şema Doğrulaması**: Argümanlar; veri tipi, enum, zorunlu alanlar ve iç içe nesne yapıları açısından denetlenir. Kural dışı çağrılar ayıklanır ve düzeltici yönergeyle modelden tek seferlik tekrar istenir.
- **Ek Alan Budama**: `additionalProperties: false` tanımlıysa, şemada yer almayan fazlalık alanlar isteği düşürmek yerine sessizce temizlenir.
- **`tool_choice` Denetimi**:
  - `"none"`: Hiçbir aracın çağrılmasına izin vermez.
  - `"required"` / `"any"`: Yanıtta mutlaka en az bir araç çağrısı bulunmasını zorunlu kılar.
  - Belirli bir fonksiyon adı: Model farklı bir araç çağırmaya çalışırsa isteği yineler.
- **Paralel Araç Denetimi**:
  - `parallel_tool_calls: false` (OpenAI) veya `tool_choice.disable_parallel_tool_use: true` (Anthropic) ayarı yapıldığında tur başına en fazla bir araç çağrısı iletilir.
- **Benzersiz Çağrı Kimlikleri**: Her araç çağrısına benzersiz bir `call_<uuid>` kimliği atanır; böylece istemcilerin mükerrer ID hatası vermesi engellenir.
- **Yerleşik M365 Araçlarını Filtreleme**: Copilot'un kendi iç araçları (`code_interpreter`, `BingWebSearch`, `trigger_plugin`) istemciye iletilmez; zira istemciler bunları çalıştıramaz.

---

## Otonom Ajan Döngüleri (Claude Code & Codex)

Otonom ajanlar, konuşma geçmişinin tamamını her adımda tekrar göndererek çok turlu döngüler yürütür. M365Bridge bu süreci şu kurallarla korur:
- **Tur Sınırı**: `M365_MAX_TOOL_ROUNDS` (varsayılan `32`, azami `512`) değeri uygulanır. Bu sınırı aşan turlar `HTTP 409 tool_round_limit` hatasıyla durdurulur.
- **Kısır Döngü Koruması**: Bir ajan aynı aracı aynı argümanlarla üst üste 3 kez çalıştırıp aynı sonucu alırsa, tekrarlanan çağrı budanarak döngü kırılır.
- **Sonuç Sıkıştırma**: Geçmişteki uzun araç çıktıları (örneğin devasa derleme logları) belirli bayt sınırlarında özetlenerek istem boyutunun şişmesi önlenir.
- **Sahte Çalıştırma Tespiti**: Model, araç çağırmak yerine metin içinde "komutu bilgisayarınızda çalıştırdım" gibi gerçek dışı ifadeler kullanırsa (Türkçe, İngilizce ve Çince denetlenir), sistem bunu yakalayarak modeli gerçek araç çağırmaya zorlar.

---

## Dahili Yerel Kodlama Araçları (İsteğe Bağlı)

M365Bridge, sunucu üzerindeki belirlenen bir çalışma dizininde kısıtlı geliştirici işlemlerini doğrudan yürütebilir.

> [!CAUTION]
> Yerel kodlama araçları, modelin `M365_WORKSPACE_DIR` dizininde dosya okuma/yazma ve kabuk komutları çalıştırmasına izin verir. Bu modu kullanırken **mutlaka** `M365_API_KEYS` ile API anahtarı koruması sağlayın.

### Etkinleştirme ve Ayarlar
`data/.env` dosyasında şu değişkenleri tanımlayın:
```env
M365_ENABLE_CODE_TOOLS=1
M365_WORKSPACE_DIR=/projenizin/dizin/yolu
M365_CODE_TOOL_TIMEOUT=30s
M365_CODE_TOOL_MAX_OUTPUT=1048576
```

### Kullanılabilir Yerel Araçlar
| Araç            | Açıklama                                                      |
|-----------------|---------------------------------------------------------------|
| `list_files`    | Çalışma dizini altındaki dosya ve klasörleri listeler         |
| `read_file`     | Dosya içeriğini okur (boyut sınırına tabidir)                 |
| `write_file`    | Çalışma dizininde dosya oluşturur veya üzerine yazar          |
| `search_files`  | Dosyalar içinde regex ile arama yapar                         |
| `git_status`    | Çalışma dizininin Git durumunu görüntüler                     |
| `git_diff`      | Değişiklik farklarını (diff) görüntüler                       |
| `git_log`       | Son commit geçmişini listeler                                 |
| `shell_command` | Çalışma dizininde kabuk komutu çalıştırır                     |
| `apply_patch`   | Birleşik (unified) Git yamalarını uygular                     |
| `run_tests`     | Belirtilen zaman ve çıktı sınırlarında test komutu çalıştırır |

---

## Model Context Protocol (MCP) Sunucusu

M365Bridge, `/mcp` rotasında JSON-RPC 2.0 (`2025-06-18` revizyonu) standardıyla bir Model Context Protocol sunucusu barındırır.

### Desteklenen MCP Araçları
- `ask_copilot`: Copilot'a bir soru yöneltip metin cevabı alır.
- `describe_image`: Bir görsel verisi (data URI) ve soru göndererek analiz yaptırır.

### MCP Örnek Çağrı
```bash
curl -s -X POST http://localhost:8230/mcp \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer api-anahtariniz" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "ask_copilot",
      "arguments": {"prompt": "Raft mutabakat algoritmasını özetle"}
    }
  }'
```
> [!NOTE]
> Benzeştirilmiş araç çağırma katmanı `/mcp` rotasında kasıtlı olarak sunulmaz. MCP istemcileri zaten kendi yerel araç motorlarına sahiptir; iki araç döngüsünü iç içe sokmak çakışmalara yol açabilir.
