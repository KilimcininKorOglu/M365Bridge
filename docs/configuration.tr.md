# Yapılandırma Kılavuzu

M365Bridge, ortam değişkenleri (environment variables) veya `data/.env` dosyası üzerinden yapılandırılır. Sistem ortamında tanımlanan değişkenler, `data/.env` dosyasındakilere göre önceliklidir. Desteklenen tüm değişkenleri ve geçerli varsayılan değerleri görmek için dilediğiniz zaman `./m365-bridge --help` komutunu çalıştırabilirsiniz.

---

## Ortam Değişkenleri

### Temel Ayarlar ve Kimlik Doğrulama
| Değişken         | Varsayılan                             | Açıklama                                                                                                                                 |
|------------------|----------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------|
| `M365_TENANT_ID` | *Zorunlu* | Microsoft Entra ID Dizin (Tenant) Kimliği (UUID). `m365-bridge login` ile otomatik oluşturulur. |
| `M365_USER_OID`  | *Zorunlu*                              | Oturum açan kullanıcının Nesne Kimliği (User Object ID, UUID). Giriş sırasında jeton taleplerinden otomatik çıkarılır.                   |
| `M365_CLIENT_ID` | `c0ab8ce9-e9a0-42e7-b064-33d422df41f1` | Sydney/Chathub erişim jetonları için kullanılan Microsoft OAuth istemci kimliği.                                                         |
| `TZ`             | Sistem saat dilimi                     | Copilot sorgularına gönderilen saat dilimi (örn. `Europe/Istanbul`, `UTC`). Sistem `/etc/localtime` dosyasına bakar, bulunamazsa UTC'yi kullanır. |

### API Güvenliği ve Erişim Kontrolü
| Değişken        | Varsayılan | Açıklama                                                                                                                     |
|-----------------|------------|------------------------------------------------------------------------------------------------------------------------------|
| `M365_API_KEYS` | *Tanımsız* | Tüm `/v1/*` rotaları ve `/mcp` için gereken virgülle ayrılmış API anahtarları. Boş bırakılırsa API dışarıya şifresiz açılır. |
| `M365_API_KEY`  | *Tanımsız* | `M365_API_KEYS` tanımlı olmadığında kullanılan tek bir API anahtarı.                                                         |

### Bağlam Penceresi ve Jeton Sınırları
| Değişken                 | Varsayılan | Açıklama                                                                                                                                       |
|--------------------------|------------|------------------------------------------------------------------------------------------------------------------------------------------------|
| `M365_CONTEXT_WINDOW`    | `1000000`  | İstemcilerin (Claude Code, Cursor vb.) istemleri vaktinden önce kırpmaması için `GET /v1/models` çıktısında duyurulan bağlam penceresi boyutu. |
| `M365_MAX_OUTPUT_TOKENS` | `1000000`  | `GET /v1/models` çıktısında duyurulan maksimum çıktı jeton tavanı.                                                                             |

### Araç Çağırma (Tool Calling) ve Ajan Döngüleri
| Değişken                 | Varsayılan | Açıklama                                                                                                                                |
|--------------------------|------------|-----------------------------------------------------------------------------------------------------------------------------------------|
| `M365_MAX_TOOL_ROUNDS`   | `32`       | Bir kullanıcı mesajı için izin verilen maksimum ardışık araç çağırma turu. Aşılırsa `HTTP 409 tool_round_limit` döner (üst sınır: 512). |
| `M365_ENABLE_WEB_SEARCH` | `1`        | Her sorguda Copilot'un yerleşik `BingWebSearch` arama yeteneğini etkinleştirir (`1` = açık, `0` = kapalı).                              |

### Dahili Kodlama Araçları (Yerel Çalıştırma)
> [!CAUTION]
> Yerel kodlama araçları modele `M365_WORKSPACE_DIR` dizininde dosya okuma/yazma ve kabuk komutları çalıştırma yetkisi verir. Etkinleştirildiğinde mutlaka API anahtarı (`M365_API_KEYS`) kullanın.
| Değişken                        | Varsayılan | Açıklama                                                                                            |
|---------------------------------|------------|-----------------------------------------------------------------------------------------------------|
| `M365_ENABLE_CODE_TOOLS`        | `0`        | Yerel araç çalıştırma yeteneği ana anahtarı (`1` açık, `0` kapalı).                                 |
| `M365_AUTO_EXPOSE_TOOLS`        | `0`        | `1` yapılırsa istemci araç tanımlamasa bile dahili kodlama araçları otomatik olarak modele sunulur. |
| `M365_WORKSPACE_DIR`            | `.`        | Dosya ve Git işlemlerinin sınırlandırıldığı çalışma dizini yolu.                                    |
| `M365_CODE_TOOL_TIMEOUT`        | `30s`      | Komut ve test çalıştırma zaman aşımı süresi (Go süre formatı: `15s`, `2m` vb.).                     |
| `M365_CODE_TOOL_MAX_OUTPUT`     | `1048576`  | Yakalanan azami komut çıktısı bayt sınırı (varsayılan 1 MiB). Aşan çıktılar kırpılır.               |
| `M365_CODE_TOOL_MAX_READ_BYTES` | `1048576`  | `read_file` aracıyla tek seferde okunabilecek maksimum dosya boyutu.                                |
| `M365_CODE_TOOL_MAX_ITERATIONS` | `10`       | İstek başına izin verilen azami model/araç döngüsü adımı.                                           |

### Çok Modlu (Multimodal) ve Görsel Üretimi
| Değişken                    | Varsayılan             | Açıklama                                                                                                                                  |
|-----------------------------|------------------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| `M365_IMAGE_HOST_ALLOWLIST` | `.officeapps.live.com` | Üretilen görsellerin indirilebileceği güvenilir sunucuların virgülle ayrılmış listesi. Nokta ile başlayanlar alt alan adlarını da kapsar. |

### Web Arayüzü ve Konuşma Kayıtları
| Değişken               | Varsayılan | Açıklama                                                                                                                            |
|------------------------|------------|-------------------------------------------------------------------------------------------------------------------------------------|
| `M365_ENABLE_WEB_UI`   | `1`        | Web arayüzünü `/` adresinde sunar ve konuşma geçmişini `data/transcripts` altına kaydeder. `0` yapılırsa ikisi de devre dışı kalır. |
| `M365_WEB_UI_PASSWORD` | *Tanımsız* | Web arayüzü giriş parolası. Boş bırakılırsa arayüz şifresiz açılır.                                                                 |

---

## Web Arayüzü Yapılandırması

Web arayüzü doğrudan Go ikili dosyasının (`pkg/webui/dist`) içine gömülüdür; harici bir Node.js süreci veya web sunucusu gerektirmez.

### Web Arayüzü Güvenlik Matrisi
`M365_WEB_UI_PASSWORD` ve `M365_API_KEYS` birbirinden bağımsız güvenlik katmanlarıdır:
- **İkisi de tanımsız**: Web arayüzü ve API şifresiz, herkese açık çalışır.
- **Yalnızca parola tanımlı**: Web arayüzü parola ister. API endpoint'leri şifresiz kalır.
- **Yalnızca API anahtarı tanımlı**: Web arayüzü geçerli bir API anahtarı girilmesini ister.
- **İkisi de tanımlı**: Web arayüzü parola ile açılır; API hem API anahtarını hem de arayüz parolasını kabul eder.

Arayüz kimlik bilgisi çerezde tutulur ve isteklere standart `Authorization: Bearer <parola>` başlığı ile eklenir; bu sayede CSRF açığı oluşmaz.

### Konuşma Geçmişi (Transcripts)
- `M365_ENABLE_WEB_UI=1` olduğunda oturum geçmişi `data/transcripts/{session_id}.json` altında saklanır.
- `0` yapıldığında `/` rotası `HTTP 404` döner, geçmiş kaydı durur ve `GET /v1/sessions/{id}/messages` endpoint'i `404 transcripts_disabled` yanıtı verir.

### Dil Desteği
Arayüz Türkçe (`tr`) ve İngilizce (`en`) dillerini destekler. Dil tercihi `m365bridge_lang` çerezinde saklanır.
Yeni bir dil eklemek için:
1. `web/src/locales/en.json` dosyasını `web/src/locales/<dil_kodu>.json` olarak kopyalayın.
2. Metinleri çevirin ve `$label` alanını güncelleyin.
3. `make ui` komutuyla arayüzü yeniden derleyin.

### Web Arayüzünü Geliştirme ve Derleme
- Anlık değişiklik izleme (hot-reload) ile arayüz geliştirmek için:
  ```bash
  cd web && npm install && npm run dev
  ```
  Vite sunucusu `http://localhost:5173` adresinde açılır ve API isteklerini `http://localhost:8230` üzerindeki köprüye yönlendirir.
- Üretim sürümünü Go kaynak koduna derlemek için:
  ```bash
  make ui
  make build
  ```
