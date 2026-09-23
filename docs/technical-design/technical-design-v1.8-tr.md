# Ege-Balancer: Go ile Modüler Bir HTTP Yük Dengeleyicinin Tasarımı ve Değerlendirmesi

**Teknik tasarım belgesi, sürüm 1.8**

| | |
| --- | --- |
| Yazar | Berk Egemen Oğuz |
| Tarih | 11 Eylül 2026 |
| Sürüm | 1.8 — v1.6'nın ve v1.7 revizyon notlarının yerini alır |
| Durum | v1.0.0 sürümünü ve sonrasındaki işleri anlatır: regresyon korumalı benchmark'lar, retry budget, power of two choices (§5.4), canlılık ve hazır olma uçları (§8.2), idempotent olmayan istekler için retry kuralı (§6.3), istek kimlikleri (§8.2) ve profilli backend'lere karşı ikinci ölçüm kampanyası (§10.8); v1.3.0'a kadar |
| Kod | `github.com/berkegemenoguz/ege-balancer` |
| Dil | Türkçe. Aynı içerikteki İngilizce sürüm: [technical-design-v1.8-en.md](technical-design-v1.8-en.md) |

---

## Özet

Bu belge, Go ile yazılmış ve on iki günlük bir plana göre inşa edilmiş bir HTTP ters proxy ve yük
dengeleyici olan Ege-Balancer'ın tasarımını, gerçekleştirimini ve değerlendirmesini anlatır.
Sistem üç seçim stratejisi (round robin, smooth weighted round robin ve least connections), aktif
ve pasif sağlık kontrolü, yapılandırılabilir üç hata stratejisi, istemci başına hız sınırlama,
bağlantı düşürmeden yapılandırma yenileme ve Prometheus tabanlı gözlemlenebilirlik sunar. Tasarımın
gerçekleştirim karşısında nasıl dayandığını raporluyoruz: hangi kararlar ayakta kaldı, hangileri
değişti ve neden. On çekirdekli tek bir makinede yapılan yük testi üç darboğaz ortaya çıkardı —
backend bağlantılarının yeniden kullanılmaması, doygunlukta sağlık kontrolünün havuzu boşaltması ve
istek başına 32 KB'lık bir bellek ayırma. Bunların giderilmesi 100 eşzamanlı bağlantıda
throughput'u saniyede 5.888 istekten 40.616 isteğe, yani yedi katına çıkardı ve p99 gecikmeyi
261 ms'den 7,7 ms'ye indirdi. Bu düzeltmelerden ikisi artık, düzeltme geri alınırsa başarısız olan
deterministik testlerle korunuyor. Sürümden sonra, uçuştaki retry sayısını uçuştaki isteklerin bir
payıyla sınırlayan bir retry budget ekledik; dört hatalı backend karşısında 50 eşzamanlı istek için
backend'lere ulaşan deneme sayısını 200'den 62'ye indirdi. Son olarak least connections'taki taramayı power of
two choices ile değiştiriyoruz — rastgele iki backend örneklemek ve daha az yüklü olanı seçmek. Bu
değişiklik havuzdaki ilk backend'e doğru olan yanlılığı ortadan kaldırdı (önce 100 ardışık isteğin
100'ü, sonra en fazla 18'i), seçim maliyetini havuz boyutundan bağımsız hale getirdi (10, 100 ve
1.000 backend'de 14 ns; taramada 1.000 backend'de 484 ns) ve sürekli yük altında yavaş backend'lerden
kaçınmayı korudu. İkinci bir ölçüm kampanyası, gerçekçi gecikme ve kapasitelere sahip backend'lere
karşı üç stratejiyi tek tip olmayan bir havuzda karşılaştırır. Eşit olmayan bir havuzda eşit pay,
throughput'u en zayıf backend ile sınırlar: round robin trafiğin %10'unu kapasitenin %3'ünü temsil
eden bir backend'e gönderir ve 100 bağlantıda isteklerin %3,6'sını reddeder; aynı noktada kapasiteye
oranlı ağırlıklar ve least connections %72 daha fazla isteği hiç ret vermeden cevaplar. Least
connections'ın sinyalini nerede yitirdiğini de gösteriyoruz: anında reddeden aşırı yüklü bir backend
uçuşta hiçbir istek tutmadığı için boşta görünür.

---

## İçindekiler

1. [Giriş](#1-giriş)
2. [Arka plan ve ilgili çalışmalar](#2-arka-plan-ve-ilgili-çalışmalar)
3. [Hedefler ve kapsam](#3-hedefler-ve-kapsam)
4. [Sistem tasarımı](#4-sistem-tasarımı)
5. [Yük dengeleme](#5-yük-dengeleme)
6. [Hata yönetimi](#6-hata-yönetimi)
7. [Kaynak koruma ve güvenlik](#7-kaynak-koruma-ve-güvenlik)
8. [Gözlemlenebilirlik](#8-gözlemlenebilirlik)
9. [Gerçekleştirim ve süreç](#9-gerçekleştirim-ve-süreç)
10. [Değerlendirme](#10-değerlendirme)
11. [Tartışma](#11-tartışma)
12. [Sınırlar ve gelecek çalışmalar](#12-sınırlar-ve-gelecek-çalışmalar)
13. [Sonuç](#13-sonuç)
- [Kaynakça](#kaynakça)
- [Ek A — Yapılandırma referansı](#ek-a--yapılandırma-referansı)
- [Ek B — Özgün tasarımdan sapmalar](#ek-b--özgün-tasarımdan-sapmalar)
- [Ek C — Sürüm geçmişi](#ek-c--sürüm-geçmişi)

---

## 1. Giriş

Bir yük dengeleyici, istemciler ile birbirinin yerine geçebilen backend'lerden oluşan bir havuz
arasında durur. Her istek için bir backend seçmeli, isteği iletmeli ve yanıtı geri döndürmelidir;
bir backend yavaşladığında, hata verdiğinde ya da ortadan kalktığında istemcinin ne göreceğine
karar vermelidir. nginx, HAProxy ve Envoy gibi olgun proxy'ler bu kararları iyi verir, ancak
davranışları büyük kod tabanlarına ve yıllar içinde birikmiş yapılandırma seçeneklerine yayılmıştır.
Bu proje küçük bir yük dengeleyiciyi temel ilkelerden kurar: her karar yazıya dökülür, ölçülebildiği
yerde ölçülür ve değiştirilebilir tutulur.

Sistem, tek satır kod yazılmadan önce bir tasarım belgesiyle (v1.6) tanımlandı, ardından on iki
günlük bir plana göre inşa edilip v1.0.0 olarak yayınlandı. Belgenin bu sürümü planın yerine
sistemin bugünkü halini anlatır; özgün gerekçeyi geçerli kaldığı yerde korur, geçerli kalmadığı
yerde bunu kayda geçirir.

Çalışmanın katkıları şunlardır:

1. **Modüler bir tasarım:** yapılandırma, seçim, sağlık kontrolü, iletme ve gözlemlenebilirlik
   küçük arayüzlerde buluşan ayrı paketlerdir; böylece her biri tek başına test edilebilir ve
   diğerlerine dokunmadan değiştirilebilir (§4).
2. **Aşırı yük altında hata yönetiminin bir incelemesi:** özgün tasarımın atladığı iki davranış
   dahil — tamamen sağlıksız görünen havuzu reddetmek yerine yine de denemek (§6.2) ve retry'ları
   yalnızca istek başına değil tüm istekler genelinde sınırlamak (§6.5).
3. **Ölçülmüş bir değerlendirme:** üç darboğazı bulan profilli bir yük testi, her birinin
   giderilmesinin etkisi, her sıcak yolun mikro benchmark'ları ve düzeltmelerden ikisini başarısız
   olabilen testlere dönüştüren regresyon korumaları (§10).
4. **Least connections için bir iyileştirme:** özgün gerçekleştirimde ölçtüğümüz bir yanlılıktan
   yola çıkan ve ona karşı değerlendirilen power of two choices (§5.4, §10.7).

Belgenin geri kalanı şöyle düzenlenmiştir. §2 tasarımı mevcut çalışmalar arasına yerleştirir. §3
hedefleri ve kapsam dışını belirtir. §4-§8 sistemi anlatır. §9 nasıl inşa edildiğini anlatır. §10
değerlendirir, §11 değerlendirmenin ne anlama geldiğini tartışır, §12 sınırlarını sıralar ve §13
sonuçlandırır.

---

## 2. Arka plan ve ilgili çalışmalar

**Ters proxy'ler.** nginx [7] ve HAProxy [8], olay güdümlü ters proxy'yi HTTP servislerinin
standart ön yüzü haline getirdi; Envoy [5] zengin dayanıklılık özellikleriyle programlanabilir bir
veri düzlemi ekledi. Go'nun standart kütüphanesi `httputil.ReverseProxy`'yi [10] sunar; iletmenin
protokol ayrıntılarını — hop-by-hop başlıklar, `X-Forwarded-For`, akış — üstlenir ve yönlendirmeyi
ve politikayı çağırana bırakır. Ege-Balancer bunun üzerine kuruludur.

**Seçim stratejileri.** *Round robin* istekleri sırayla dağıtır. *Weighted round robin* bunu
yapılandırılmış ağırlıklarla orantılı yapar; nginx'in *smooth* çeşidi [7] ağır bir backend'in
sıralarını bir araya yığmak yerine araya serpiştirir. *Least connections* her isteği uçuşta en az
isteği olan backend'e gönderir ve hızı farklı backend'lere uyum sağlar; HAProxy buna `leastconn`
der [8]. En küçüğü bulmak için tüm backend'leri taramak O(n)'dir ve §5.3'te gösterildiği gibi
eşitliklerin nasıl çözüldüğüne duyarlıdır. *Power of two choices* [1, 2] rastgele iki backend
örnekler ve daha az yüklü olanı seçer: top-kutu (balls-into-bins) modelinde en yüksek yükü
Θ(log n / log log n)'den Θ(log log n)'ye indirir — ikinci bir örnekten gelen üstel bir iyileşme —
ve yük bilgisi eskidiğinde de zarifçe bozulur [3]. Envoy'un varsayılan least request dengeleyicisi
bunu kullanır [5].

**Hata yönetimi.** Sağlık kontrolü, probe'lara (aktif) ya da gerçek trafiğe (pasif) cevap
veremeyen backend'leri havuzdan çıkarır. Sağlık kontrolü *tüm* backend'leri çıkardığında Envoy'un
*panic threshold*'u yine de tüm havuza yönlendirir [5]; gerekçesi, ölü sanılan bir havuzun yalnızca
yavaş olabileceğidir. *Devre kesici* (circuit breaker) [9], tekrar tekrar başarısız olan bir
backend'e trafik göndermeyi durdurur ve bir bekleme süresinden sonra tek bir sınama isteğini
geçirir. Retry'lar geçici hataları gizler ama yükü tam da backend'ler hata verirken çoğaltır; SRE
literatürü bunları bir *retry budget* ile sınırlamayı önerir [4]. Envoy bunu aktif isteklerin bir
payı olarak [5], Finagle ise bir zaman penceresi üzerinde token bucket olarak [6] ifade eder. Dean
ve Barroso aynı gerilimi kuyruk gecikmesi tarafından ele alır [11].

**Kuyruk teorisi.** Little yasası [12] — sistemdeki ortalama istek sayısı, varış hızı ile sistemde
geçen ortalama sürenin çarpımına eşittir — §10'daki iki gözlemi açıklar: throughput doyduktan sonra
gecikmenin eşzamanlılıkla doğrusal büyümesi ve backend'ler mikrosaniyeler içinde cevap verdiğinde
least connections'ın dengeleyecek bir şeyi kalmaması.

---

## 3. Hedefler ve kapsam

### 3.1 Fonksiyonel hedefler

- Gelen HTTP isteklerini bir backend havuzuna dağıtmak.
- Yapılandırmadan seçilen üç seçim stratejisi: round robin, least connections ve weighted round
  robin.
- Aktif ve pasif sağlık kontrolü: sağlıksız bir backend'i çıkarmak, iyileşince geri almak.
- Yapılandırılabilir hata stratejileri: başka bir backend'de yeniden denemek, hemen hata dönmek ya
  da devre kesmek.
- Graceful shutdown ve bağlantı düşürmeden `SIGHUP` ile yapılandırma yenileme.
- Yapılandırılmış loglar, Prometheus metrikleri ve insanın okuyabileceği bir durum ucu.

### 3.2 Hedef ortam

Tek makinede, birkaç backend sürecinin önünde çalışan küçük-orta ölçekli bir yük dengeleyici.
Referans ortam tek bir geliştirici makinesinde Docker Compose ile çalışır: on mock backend, yük
dengeleyici, Prometheus ve Grafana. "Production'a hazır" ifadesi gerçek bir sunucuya taşınmaya hazır
olmayı anlatır — küçük, root olmayan bir imaj, sağlık uçları, graceful shutdown, CI — bir dağıtımın
var olduğunu değil.

### 3.3 Kapsam dışı

- TLS sonlandırma, HTTP/2 ve gRPC.
- Dağıtık ya da çok düğümlü dengeleme ve servis keşfi.
- Sticky session: backend'lerin durumsuz olduğu varsayılır.
- Elle yazılmış bir epoll olay döngüsü. Özgün tasarım bunu opsiyonel bir öğrenme egzersizi olarak
  planlamıştı; yapılmadı ve sistemde hiçbir şey ona bağlı değil (§4.3).

---

## 4. Sistem tasarımı

### 4.1 Mimari

Bir istek sabit bir zincirden geçer. Listener bağlantıyı bir bağlantı sınırı altında kabul eder;
hız sınırlayıcı ve istek doğrulama onu reddedebilir; proxy çekirdeği sağlıklı ve devresi kapalı
olanlar arasından bir backend seçer, isteği iletir ve deneme başarısız olursa hata stratejisini
uygular. Sağlık kontrolü zincirin yanında çalışıp onu besler; metrikler onu gözlemler.

```mermaid
flowchart LR
    C[İstemci] --> L["Listener<br/>bağlantı sınırı, zaman aşımları"]
    L --> R["Hız sınırlayıcı<br/>IP başına token bucket"]
    R --> V["İstek doğrulama<br/>çerçeveleme, gövde boyutu"]
    V --> P["Proxy çekirdeği"]
    P --> F{"Uygun backend'ler<br/>sağlıklı, devre kapalı,<br/>henüz denenmemiş"}
    F --> S["Strateji<br/>RR · WRR · LC"]
    S --> B["İlet<br/>httputil.ReverseProxy"]
    B -->|başarılı| C
    B -->|başarısız| RB{"Hata stratejisi<br/>ve retry budget"}
    RB -->|yeniden dene| F
    RB -->|vazgeç| U["503 + Retry-After"]
    H["Sağlık kontrolü<br/>aktif probe'lar"] -.-> F
    B -. "sonuç (pasif)" .-> H
    P -.-> M["Metrikler · loglar · /status"]
```

*Şekil 1 — İsteğin yolu. Düz oklar isteği, noktalı oklar isteğe giren ve ondan çıkan durumu
gösterir.*

### 4.2 Paketler ve arayüzler

```
cmd/lb/                    giriş noktası: yapılandırmayı okur, logger'ı kurar, uygulamayı çalıştırır
cmd/demo/                  demo ortamını yönetmek için yerel konsol (imaja dahil değil)
cmd/mockbackend/           demo ortamı için mock backend (imaja dahil değil)
internal/app/              kurulum (wiring); binary ve entegrasyon testleri ortak kullanır
internal/config/           ayrıştırma, varsayılanlar, doğrulama, yenileme kuralları
internal/balancer/         Backend, LBStrategy arayüzü ve üç strateji
internal/health/           aktif ve pasif sağlık kontrolü
internal/proxy/            iletme, hata stratejileri, retry budget, hız sınırlama, doğrulama
internal/server/           listener'lar, bağlantı sınırı, graceful shutdown
internal/observability/    yapılandırılmış loglama, Prometheus metrikleri, /status, pprof
internal/integration/      gerçek soketler üzerinde uçtan uca testler
```

Kurulum `main` yerine `internal/app` içinde durur; böylece entegrasyon testleri binary'nin çalıştırdığı
zincirin tam kendisini kurar. `cmd/lb/main.go` yalnızca yapılandırmayı okur, logger'ı kurar ve
`app.New`'u çağırır. Paketler iki arayüzde buluşur:

```go
// internal/balancer
type LBStrategy interface {
    Select(backends []*Backend) (*Backend, error)
    Name() string
}

// internal/health
type Checker interface {
    Start(ctx context.Context, backends []*Backend)
    IsHealthy(addr string) bool
    ReportSuccess(addr string)
    ReportFailure(addr string)
    Reload(ctx context.Context, cfg config.HealthCheck, backends []*Backend)
}
```

Bir `Backend` adresini ve iki atomik değeri taşır: ağırlığını — istekler üzerinde dengelenirken bir
yenileme onu değiştirebilir — ve least connections'ın okuduğu uçuştaki istek sayısını. Sağlık
bilinçli olarak `Backend`'in bir alanı *değildir*: adrese göre checker'da tutulur; böylece aktif ve
pasif kontrol aynı sayaçları besler ve sağlık durumu havuz yeniden kurulduğunda korunur.

### 4.3 Eşzamanlılık modeli

Her bağlantı kendi goroutine'inde işlenir; kod senkron yazılırken Go runtime'ının netpoller'ı
soketleri Linux'ta epoll üzerinden çoklar. Özgün tasarım, öğrenme amacıyla bir epoll döngüsünün elle
yazılacağı ikinci, opsiyonel bir aşama öneriyordu; değerlendirme (§10.2) kalan CPU profilinin sistem
çağrılarının hakimiyetinde olduğunu ve yük dengeleyicinin kendi kodunda sıcak nokta kalmadığını
gösterdi. Bu da ona dönmek için performans açısından bir neden bırakmıyor.

Paylaşılan durum, nasıl kullanıldığına göre korunur:

| Durum | Koruma | Neden |
| --- | --- | --- |
| Round robin konumu | tek bir atomik sayaç | seçim başına tek bir artırım |
| Backend başına uçuştaki istek | `Backend` üzerinde atomik sayaç | least connections okur, her istek yazar |
| Backend ağırlığı | `Backend` üzerinde atomik | okunurken yenileme ile değişir (§11.1) |
| Weighted round robin kredisi | mutex | tüm havuz üzerinde oku-değiştir-yaz |
| Sağlık durumu | checker'da read-write mutex | her istekte okunur, probe'lar ve raporlar yazar |
| Devre durumu | breaker'da mutex | küçük; backend hata verirken yazma ağırlıklı |
| İletme ayarları | değişmez bir anlık görüntüye `atomic.Pointer` | yenilemede bütünüyle değiştirilir |
| Retry budget | iki atomik sayaç | her istekte; kilitsiz (§6.5) |

### 4.4 Yapılandırma ve yenileme

Yapılandırma, katı biçimde çözülen bir YAML dosyasıdır — bilinmeyen bir alan hatadır — ardından
varsayılanlar uygulanır ve doğrulanır. Doğrulama ilk sorunu değil tüm sorunları birlikte raporlar.
Tam şema Ek A'dadır.

`SIGHUP` geldiğinde dosya yeniden okunur ve doğrulanır. Geçersizse hiçbir şey değişmez; hata
loglanır ve sayılır. Geçerliyse iletme ayarları yeni ve değişmez bir anlık görüntü olarak yeniden
kurulur ve tek bir atomik yazımla yayınlanır. Bir istek anlık görüntüyü başlarken bir kez okur ve
sonuna kadar onu kullanır; böylece bir yenileme hiçbir isteği yeni havuz ile eski hata stratejisi
arasında bırakamaz. Yenilemeden sağ çıkan backend'ler `Backend` nesnelerini — dolayısıyla uçuştaki
istek sayılarını — ve sağlık serilerini korur. Bir sokete bağlı ayarlar (`listen_addr`,
`metrics_addr`, `max_connections`, zaman aşımları, `enable_pprof`) servis sırasında değişemez;
yenileme geri kalan her şeyi uygular ve bunlardan hangilerine dokunmadığını loglar.

---

## 5. Yük dengeleme

Tüm stratejiler `LBStrategy`'yi gerçekler. Proxy çekirdeği önce havuzu sağlıklı, devresi kapalı ve
bu isteğin henüz denemediği backend'lere daraltır; strateji bunlar arasından seçer. Filtrelemenin
stratejilerin dışında tutulması, her stratejiyi kendisine verilen havuzun saf bir fonksiyonu olarak
bırakır.

### 5.1 Round robin

Her seçimde tek bir atomik sayaç artırılır ve havuz boyutuna göre modu alınır. Seçim O(1) ve
kilitsizdir. Backend'ler eşit olduğunda doğru seçimdir ve kesindir: on backend üzerinde, onun katı
sayıda istek eşit bölünür.

### 5.2 Smooth weighted round robin

Özgün tasarım ağırlıklı seçimi olasılıksal olarak tanımlıyordu. Gerçekleştirim bunun yerine nginx'in
deterministik smooth weighted round robin'ini [7] kullanır. Her seçimde:

1. her backend'in kredisi ağırlığı kadar artırılır;
2. en yüksek krediye sahip backend seçilir;
3. onun kredisi tüm ağırlıkların toplamı kadar azaltılır.

Sonuç, yapılandırılan oranı ortalamada değil tam olarak tutturur ve ağır bir backend'in sıralarını
döngüye yayar: 5, 1, 1 ağırlıkları `a a a a a b c` yerine `a a b a c a a` üretir. Rastgelelik kaynağı
olmadığından testler istatistiksel değil kesindir — 1, 2 ve 3 ağırlıkları üzerinde 1.200 istek tam
olarak 200, 400 ve 600'e bölünür. Kredi adrese göre tutulur; böylece backend'ler havuzdan çıkıp geri
döndükçe stratejiye sunulan havuz değişse de anlamını korur. Bedeli, bir mutex altında havuzun
taranmasıdır: seçim başına O(n) (§10.4).

### 5.3 Least connections

Her istek, iletilmeden önce backend'inin uçuştaki istek sayacını artırır ve bittiğinde — retry'lar
dahil — azaltır. v1.0.0'a kadar least connections havuzu tarayıp sayısı en düşük olan backend'i
döndürüyordu; eşitlikte havuzdaki ilk backend seçiliyordu.

Hızı farklı backend'lere uyum sağlıyordu; entegrasyon testleri bunu gösterdi: bir yavaş ve iki hızlı
backend ile 60 eşzamanlı istekten yavaş olan 16, hızlı ikili 44 istek aldı. İki zayıflığı vardı ve ikisi de bu projede görüldü:

- **İlk backend'e doğru bir yanlılık.** Backend'ler istekler geldiğinden daha hızlı cevap verdiğinde
  eşitlikler sık olur. Little yasasına göre saniyede 20 istek ve istek başına bir milisaniyede
  uçuştaki ortalama istek sayısı 0,02'dir; yani neredeyse her seçim tüm sayaçları sıfır görür ve ilk
  backend'i seçer. On eşit backend ile gerçek soketler üzerinde ölçüldüğünde 100 ardışık isteğin
  tamamı `backend-1`'e gitti; 100 eşzamanlı istekten ona 16, diğerlerinin her birine 7 ile 14
  arasında düştü.
- **Sürü davranışı (herding).** Birlikte gelen istekler, hiçbiri sayacı artırmadan önce aynı
  sayaçları okur ve hepsi aynı backend'i seçer.

Seçim ayrıca O(n)'di: on backend üzerinde 3,9 ns, yüz backend üzerinde 47 ns, bin backend
üzerinde 484 ns. Bu maliyet
iletmenin yanında küçüktür, ama hiçbir fayda sağlamadan havuzla birlikte büyür.

### 5.4 Power of two choices

> **Durum:** v1.1.0 için gerçekleştirildi; §10.7 onu yerini aldığı taramaya karşı değerlendirir.

**Algoritma.** n backend'lik bir havuz için:

- n = 0: backend yok (bugünkü gibi 503);
- n = 1: o backend;
- n ≥ 2: rastgele ve düzgün dağılımlı iki *farklı* indeks çek, iki backend'in uçuştaki istek
  sayılarını karşılaştır, düşük olanı döndür; eşitlikte ilk çekileni döndür.

Farklı indeksler `i = rand(n)`, `j = rand(n−1)` ve `j ≥ i` ise `j++` biçiminde çekilir; tekrar
döngüsü gerekmez. n = 2 iken iki örnek havuzun tamamıdır, dolayısıyla seçim tam olarak least
connections'tır. Strateji, Envoy'un least request dengeleyicisinin yaptığı gibi yapılandırmadaki
`least_connections` adını korur; yapılandırmalar değişmez.

**§5.3'teki sorunları neden çözer.**

- *Yanlılık ortadan kalkar.* Tüm sayılar eşit olduğunda seçim, her zaman ilk backend yerine, iki
  rastgele çekilişin ilkidir — havuz üzerinde düzgün dağılımlı.
- *Sürü davranışı azalır.* Eşzamanlı istekler farklı çiftler örnekler; aynı sayaçları okusalar bile
  dağılırlar. Bu, Mitzenmacher'ın eskimiş yük bilgisi analizinin [3] ortamıdır: orada iki seçenek
  örneklemek etkili kalırken, eski bilginin küresel en küçüğünü seçmek kötü davranır.
- *Yük dengeli kalır.* Top-kutu modelinde iki seçenekle en yüksek yük, tek rastgele seçimdeki
  Θ(log n / log log n)'e karşı Θ(log log n)'dir [1, 2]; ikinci örnek, tam taramanın faydasının
  neredeyse tamamını sağlar.
- *Seçim O(1) olur:* havuz boyutu ne olursa olsun iki rastgele sayı ve iki atomik okuma.

**Rastgelelik ve testler.** Strateji, eşzamanlı kullanım için güvenli olan ve çağırandan kilit
gerektirmeyen `math/rand/v2` üst düzey fonksiyonlarından çeker. Deterministik testler özgün
gerçekleştirimin bir ilkesiydi (§5.2); bunu korumak için rastgele indeks kaynağı stratejinin bir
alanıdır ve birim testlerinde tohumlanmış bir üreteçle kurulur, böylece başarısız bir test yeniden
üretilebilir.

**Neler değişmez.** Filtreleme (sağlık, devre, daha önce denenmiş olma) stratejiden önce yapılır;
dolayısıyla bir örnek hiçbir zaman uygun olmayan bir backend'e düşemez. Weighted round robin ve
round robin etkilenmez.

---

## 6. Hata yönetimi

### 6.1 Sağlık kontrolü

*Aktif* kontrol her `interval`'de her backend'in `health_check.path` yolunu kendi `timeout`'u ile
yoklar. *Pasif* kontrolü proxy yürütür ve her denemenin sonucunu bildirir. İkisi de backend başına
tek bir ardışık sonuç sayacı çiftini besler: art arda `unhealthy_threshold` başarısızlık onu
çıkarır, art arda `healthy_threshold` başarı geri alır. Böylece bir backend, bir sonraki probe'u
beklemeden, gerçek trafik onda başarısız olur olmaz havuzdan çıkar. Backend'ler sağlıklı başlar ki
trafik ilk probe tamamlanmadan akabilsin; checker'ın tanımadığı bir adres sağlıklı sayılır.

### 6.2 Tamamen sağlıksız bir havuz yine de denenir

Özgün tasarım, sağlık kontrolü tüm backend'leri çıkardığında ne yapılacağını söylemiyordu. Yük testi
bunu cevapladı: doygunlukta probe'lar zaman aşımına uğrayan ilk istekler arasındaydı, tüm backend'ler
aynı anda sağlıksız işaretlendi — 172 çıkarma ve bunlarla eşleşen 172 geri dönüş — ve yük dengeleyici,
gerçekte hiçbir şey çökmemişken 275.769 isteği reddetti. Yavaş bir sistem bozuk bir sisteme dönmüştü.

Havuzda uygun hiçbir şey kalmadığında istek artık yine de denenmemiş backend'lere sunulur ve bu geri
düşüş `no_healthy_backend` olarak sayılır. Bu, Envoy'un panic mode'udur [5]. Hâlâ cevap
verebilecek bir backend bir denemeye değer; gerçekten ölü olan bir başarısız denemeye mal olur ve
ardından istemci zaten alacağı 503'ü alır. Gözlemlenebilir sonucu: tüm backend'ler sağlıksız ama
hâlâ cevap verir durumdayken istemci backend'in kendi yanıtını alır.

### 6.3 Hata stratejileri

Başarısız bir deneme; reddedilen ya da zaman aşımına uğrayan bir bağlantı, `response_timeout`
içinde cevap vermeye başlamayan bir backend ya da — yalnızca `retry_on_5xx` açıksa — bir 5xx
yanıttır.

| Strateji | Başarısız bir denemede davranış |
| --- | --- |
| `retry_next_backend` | bu isteğin henüz denemediği başka bir backend'i, en fazla `max_retries` kez ve retry budget dahilinde dener (§6.5) |
| `fail_fast` | istemciye hemen yanıt verir |
| `circuit_breaker` | istemciye hemen yanıt verir ve hatayı backend'in devresini açmaya doğru sayar |

Bir retry, aynı istek içinde hiçbir zaman bir backend'i tekrar etmez. Yeniden gönderilebilmesi için
istek gövdesi, retry mümkün olduğunda `max_request_body_bytes`'a kadar tamponlanır; tek denemede
doğrudan akıtılır.

Bir isteğin retry edilip edilebileceği ise metoduna bağlıdır. RFC 9110'un tanımladığı idempotent
istekler — GET, HEAD, PUT, DELETE, OPTIONS, TRACE — her hatadan sonra retry edilir. Olmayanlar,
yani POST, PATCH ya da yük dengeleyicinin tanımadığı bir metot, yalnızca backend'e bağlantı hiç
kurulamadığında retry edilir: hiçbir backend'in isteği görmediğini kanıtlayan tek hata budur.
Sonraki her hata — `retry_on_5xx` açıkken gelen bir 5xx yanıt dahil — isteği `not_retryable` olarak
sayılan bir 503 ile bitirir; çünkü backend isteği yerine getirmiş ve yanıtını sonra kopan bir
bağlantıya yazmış olabilir, ikinci bir deneme ikinci bir sipariş oluşturur (Ek B, 15. madde).

### 6.4 Devre kesici

```mermaid
stateDiagram-v2
    [*] --> Kapalı
    Kapalı --> Kapalı: başarı (seri sıfırlanır)
    Kapalı --> Açık: art arda failure_threshold hata
    Açık --> YarıAçık: open_duration doldu
    YarıAçık --> Kapalı: sınama isteği başarılı
    YarıAçık --> Açık: sınama isteği başarısız
```

*Şekil 2 — Bir backend'in devresi. Açık durumdayken ve yarı açık sınama isteği uçuştayken backend
seçimden çıkarılır.*

Devre kesici sağlık kontrolünden bağımsızdır: sağlık, bir backend'in probe'lara cevap verip
vermediğini; devre ise gerçek trafikte hata verip vermediğini yansıtır. Sınama isteği olarak tam bir
istek geçirilir; diğer her şey onun sonucunu bekler.

### 6.5 Retry budget

`max_retries` bir isteğin retry'larını sınırlar, istekler genelindeki retry'ları sınırlamaz: bir
havuz hata vermeye başladığında her istek aynı anda yeniden dener ve backend'lere ulaşan trafik
1 + `max_retries` katına kadar büyür — tam da en az kaldırabilecekleri anda. Bu, SRE literatürünün
uyardığı retry fırtınasıdır [4].

Bu yüzden yük dengeleyici, Envoy'u [5] izleyerek bir budget tutar. Uçuştaki istekleri ve uçuştaki
retry'ları sayar ve bir retry'a yalnızca şu koşul sağlanırken izin verir:

```
uçuştaki_retry < max(min_retry_concurrency, budget_percent × uçuştaki_istek)
```

Varsayılanlar %20 ve 3'tür. Alt sınır, yükün %20'sinin sıfıra yuvarlandığı hafif trafikte
retry'ları mümkün tutar. Budget'ın reddettiği retry gönderilmez: istemci `Retry-After` ile 503
alır ve ret `retry_budget_exhausted` olarak sayılır.

Bu modeli Finagle'ın token bucket'ına [6] tercih ettik, çünkü ayarlanacak bir zaman penceresi ya da
dolum hızı yoktur, yükü her an izler ve iki atomik sayaçla, kilitsiz çalışır. Budget, yapılandırma
anlık görüntüsüne aittir: bir yenileme yeni ve boş bir budget kurar, uçuştaki istekler başladıkları
budget'ı korur; böylece hiçbir sayaç yanlış örnek üzerinde azaltılmaz. Budget varsayılan olarak
açıktır; ondan söz etmeyen bir yapılandırma varsayılanları alır ve hafif trafik tam olarak eskisi
gibi retry yapar.

### 6.6 İstemcinin gördüğü

| Durum | Yanıt | Ret nedeni |
| --- | --- | --- |
| İstemci başına hız aşıldı | 429, `Retry-After: 1` | `rate_limited` |
| Belirsiz çerçeveleme (iki `Content-Length` ya da `Content-Length` ile `Transfer-Encoding`) | 400 | `bad_framing` |
| `max_request_body_bytes`'ı aşan gövde | 413 | `body_too_large` |
| Tüm denemeler başarısız ya da denenecek backend yok | 503, `Retry-After: 5` | `no_backend_available` |
| Budget'ın reddettiği retry | 503, `Retry-After: 5` | `retry_budget_exhausted` |
| Backend'in yerine getirmiş olabileceği, başarısız bir POST ya da PATCH | 503, `Retry-After: 5` | `not_retryable` |
| Backend 5xx döndü, `retry_on_5xx: false` | backend'in kendi yanıtı | — |
| Tüm backend'ler sağlıksız ama cevap veriyor | backend'in kendi yanıtı | `no_healthy_backend` (sayılır, reddedilmez) |

---

## 7. Kaynak koruma ve güvenlik

Bu sürümde güvenlik, ağır bir katmandan çok, ucuz ama etkisi büyük önlemlerden oluşur.

- **Bağlantı sınırı.** Trafik listener'ı `max_connections`'ın ötesindeki bağlantıları reddeder.
  Gözlemlenebilirlik sunucusunda sınır yoktur; böylece tam da trafik portu dolduğunda cevap vermeye
  devam eder.
- **Her aşamada zaman aşımı.** Bağlantı, yanıt, okuma, yazma ve boşta kalma sürelerinin her birinin
  bir sınırı vardır; yavaş bir istemci ya da backend kaynakları süresiz tutamaz.
- **İstemci başına hız sınırlama.** İstemci adresi başına bir token bucket; saniyede
  `rate_limit_per_ip` kadar dolar ve aynı büyüklükte bir patlamaya izin verir. Adres, istemcinin
  taklit edebileceği bir başlık değil, soketin karşı ucudur. 10.000 istemci izlendiğinde on dakika
  boşta kalan bucket'lar süpürülür; böylece tek seferlik istemciler belleği sınırsız büyütemez. Bir
  yenileme, bucket'ları sıfırlamadan hızı değiştirir.
- **Request smuggling.** Gövde çerçevelemesi iki farklı biçimde okunabilecek istekler, bir backend
  onları görmeden RFC 9112'yi [13] izleyerek reddedilir (§6.6). Go'nun sunucusu en açık durumları
  zaten reddeder; kontrol, proxy buna bağımlı olmasın diye tekrarlanır.
- **Yönlendirme başlıkları.** `X-Forwarded-For` ve ilgili başlıklar eklenmez, yeniden yazılır;
  böylece bir istemci backend'in göreceği adresi taklit edemez.
- **Ayrı gözlemlenebilirlik portu.** `/metrics`, `/status`, `/healthz`, `/readyz` ve opsiyonel
  pprof uçları trafik portunda değil, `metrics_addr` üzerinde sunulur. pprof, heap ve goroutine durumunu açığa çıkardığı için
  varsayılan olarak kapalıdır.
- **Tedarik zinciri ve imaj.** CI her push'ta `govulncheck` çalıştırır. İmaj iki aşamada derlenir ve
  `distroless/static:nonroot` üzerinde çalışır — 22,6 MB, kabuk yok, paket yöneticisi yok, root
  olmayan kullanıcı.

---

## 8. Gözlemlenebilirlik

### 8.1 Metrikler

| Metrik | Tür | Etiketler | Anlamı |
| --- | --- | --- | --- |
| `lb_requests_total` | counter | backend, status | bir backend'in cevapladığı istekler |
| `lb_request_duration_seconds` | histogram | backend | yük dengeleyicide ölçülen servis süresi |
| `lb_backend_failures_total` | counter | backend | bir backend'in karşılayamadığı denemeler |
| `lb_retries_total` | counter | — | gönderilen retry'lar |
| `lb_rejected_requests_total` | counter | reason | yük dengeleyicinin kendisinin reddettiği istekler (§6.6) |
| `lb_config_reloads_total` | counter | result | uygulanan ya da reddedilen yenilemeler |
| `lb_backend_active_connections` | gauge | backend | uçuştaki istekler, scrape anında okunur |
| `lb_backend_healthy` | gauge | backend | 1 sağlıklı, 0 değil; scrape anında okunur |

İki gauge, ayrı değişkenlere kopyalanmak yerine Prometheus scrape ettiğinde canlı durumdan okunur;
böylece zamanla sapabilecek ikinci bir kopya yoktur.

### 8.2 Durum, loglar ve profilleme

`/status` bir scraper'a değil bir insana cevap verir: algoritma, sağlıklı backend sayısı, uygulanan
yenileme sayısı ve her backend'in ağırlığı, sağlığı ve uçuştaki istek sayısı. Loglar `log/slog`
üzerinden yapılandırılmış JSON (ya da metin) olarak yazılır. Etkinleştirildiğinde `/debug/pprof`
metrik portunda sunulur.

Her istek, bu logları backend'in kendi loglarına bağlayan bir kimlik taşır. Kimlik, istemcinin
`X-Request-Id` başlığı en fazla 64 karakterlik yazdırılabilir ASCII ise ondan alınır — böylece yük
dengeleyiciden önce başlamış bir iz burada kopmaz — değilse `crypto/rand` ile üretilir. İstemciye
geri döner, aynı başlıkla backend'e iletilir ve loglara `request_id` olarak yazılır. En dışta
atanır, yani hız sınırlama ve doğrulamadan önce; böylece yük dengeleyicinin kendisinin reddettiği
bir istek de ilettiği bir istek kadar izlenebilir (Ek B, 16. madde).

İki uç daha bir insana değil bir orkestratöre cevap verir. `/healthz` canlılığı bildirir ve süreç
cevap verebildiği sürece 200 döner. `/readyz` hazır olmayı bildirir: `/status`'un okuduğu aynı
sağlık kontrolüne göre en az bir backend sağlıklı olduğu sürece 200, hiçbiri sağlıklı değilse ya da
kapanma başladıysa 503 döner. İkisi bilerek ayrı tutulur: bütün backend'leri düşmüş bir yük
dengeleyici hâlâ çalışmaktadır ve onu yeniden başlatmak hiçbir backend'i geri getirmez; bu yüzden
yalnızca hazır olma durumu havuza bağlıdır.

Kapanırken yük dengeleyici önce kendini hazır değil olarak bildirir, sonra trafik portunu boşaltır
ve metrik portunu ancak trafik portu boşaldıktan sonra kapatır. İkisini birlikte kapatmak, önceden
olduğu gibi, `/readyz`'i söyleyecek bir şeyi olduğu tek anda ortadan kaldırıyordu. Distroless
imajda bir container sağlık kontrolünü çalıştıracak kabuk ya da `curl` yoktur; bu yüzden binary
kendini kontrol eder: `lb -probe <url>` 200 gelirse 0, gelmezse 1 ile çıkar ve Compose dosyası
bunu `/healthz` üzerinde çalıştırır (Ek B, 14. madde).

### 8.3 İzleme yığını

Compose ortamı, beş saniyede bir scrape eden Prometheus'u ve birbirine bağlı üç hazır dashboard'lu Grafana'yı çalıştırır. *Overview* ekranda açık tutulacak
olandır: öne çıkan değerler, backend başına istek oranı ve trafik payı, gecikme ve bir sağlık
şeridi. *Backends* backend'leri karşılaştırır — bir tablo, trafik payı, bir pencere üzerinden
ortalanmış uçuştaki istekler, backend başına p95 gecikme ve bir gecikme ısı haritası.
*Resilience* hataların etkisini gösterir: durum sınıfına göre yanıtlar, nedenlerine göre retler,
budget'a karşı retry'lar ve yenilemeler.

Üç seçim onları okunur kılar. Her grafik yapılandırma yenilemelerini işaretler; böylece bir
değişikliğin etkisi yapıldığı ana göre görülür. Her backend profilinin rengini korur (§9.4). Ve
uçuştaki istek göstergesi ortalanmış gösterilir: hafif yükte beş saniyede bir örneklendiğinde çoğu
zaman 0 ya da 1 okunur ve bir backend'in ne kadar meşgul olduğu hakkında bir şey söylemez.
Dashboard'lar bir betikle üretilir, böylece üçü tutarlı kalır. Bu yığını çalıştırmaktan çıkan iki
kaynak bulgusu Ek B'de (7. madde) kayıtlıdır: Grafana daha yüksek bir sınır yerine bir Go bellek
bütçesine (`GOMEMLIMIT`) ihtiyaç duyar; Prometheus 256 MB'a sığar.

---

## 9. Gerçekleştirim ve süreç

### 9.1 Plan ve nasıl gerçekleşti

| Gün | Planlanan | Teslim edilen |
| --- | --- | --- |
| 1 | Proje kurulumu, mock ortam | repo iskeleti, CI, on `http-echo` backend |
| 2 | Yapılandırma modülü | katı YAML ayrıştırma, varsayılanlar, tüm sorunları raporlayan doğrulama |
| 3 | Listener ve proxy çekirdeği | bağlantı sınırlı listener, graceful shutdown, iletme |
| 4 | Round robin | `LBStrategy`, round robin |
| 5 | Least connections, weighted round robin | uçuştaki istek sayaçları, *smooth* weighted round robin |
| 6 | Sağlık kontrolü | ortak sayaçlar üzerinde aktif ve pasif kontrol |
| 7 | Proxy çekirdeğinin tamamlanması | doğrulama, hız sınırlama, zaman aşımları, hata stratejileri, devre kesici |
| 8 | Loglama ve metrikler | slog, Prometheus, `/status`, Prometheus ve Grafana yığını |
| 9 | Entegrasyon testleri | gerçek soketler üzerinde uçtan uca test paketi |
| 10 | Yük testi ve profilleme | üç darboğaz bulundu ve giderildi; performans raporu |
| 11 | Dayanıklılık ve yenileme | hata senaryoları doğrulandı, `SIGHUP` ile yenileme, yanıt zaman aşımı |
| 12 | Production hazırlığı, sürüm | distroless imaj, release workflow'u, dağıtım kontrol listesi, v1.0.0 |

10. ve 11. günlerin yanında planlanan opsiyonel epoll egzersizi yapılmadı (§4.3).

### 9.2 İş akışı

Özgün tasarım korumalı bir `main`, feature dalları ve pull request'ler öngörüyordu. Tek geliştirici
ve incelemeci olmadığında bu, kalite kapısı olmadan süreç yükü ekler; bu yüzden çalışma doğrudan
`main`'e commit edilir ve kapı CI'dır: her push gofmt, `go vet`, golangci-lint, govulncheck,
derleme, race detector altında tüm test paketi ve imaj derlemesini çalıştırır. Bir `vX.Y.Z` etiketi
release workflow'unu tetikler; bu, testleri yeniden çalıştırır, imajı derler, onu etiket ve `latest`
olarak gönderir ve sürüm notlarını yayınlar. Commit mesajları Conventional Commits'e uyar. Bir ekip
oluşursa pull request akışı yeniden açılabilir; CI, onun ihtiyaç duyacağı her şeyi zaten çalıştırır.

İkinci bir workflow her push'ta benchmark'ları çalıştırır, aynı runner'da push'tan önceki kod üzerinde
yeniden çalıştırır ve benchstat [14] karşılaştırmasını çalışma özetine yazar. Yalnızca raporlar,
hiçbir zaman başarısız olmaz: paylaşılan runner'larda zamanlamalar aynı çalıştırmalar arasında
yüzde birkaç oynar.

### 9.3 Test

Test paketi, 27'si birleştirilmiş yük dengeleyiciyi gerçek soketlerde başlatan entegrasyon testi
olmak üzere 137 test fonksiyonu ve 10 benchmark içerir. Paket bazında birim test kapsamı:

| Paket | Kapsam |
| --- | --- |
| `balancer` | %100,0 |
| `observability` | %98,8 |
| `health` | %97,9 |
| `proxy` | %94,3 |
| `config` | %90,3 |
| `server` | %83,7 |

Entegrasyon testleri yapılandırmalarını bir dosyaya yazar ve binary ile aynı yoldan yükler; böylece
varsayılanlar ve doğrulama da diğer her şeyle birlikte sınanır.

### 9.4 Demo ortamı

v1.1.0'a kadar Compose ortamındaki on backend, her isteğe anında sabit bir metinle cevap veren
`hashicorp/http-echo` idi. Buna bağlı üç şey bundan zarar gördü: least connections'ın dengeleyecek
bir şeyi yoktu, yük testi on baytlık gövdeler iletti ve tüm backend'ler eşitti. Artık hepsi tek bir
program, `cmd/mockbackend`, her backend için bir profille çalıştırılıyor:

| Ayar | Anlamı |
| --- | --- |
| `latency`, `latency-p99` | bir isteğin servis süresinin medyanı ve 99. yüzdeliği; log-normal dağılımdan çekilir |
| `capacity`, `queue` | aynı anda işlenen istekler ve bekleyebilecek istekler; kuyruğun ötesinde backend 503 döner |
| `body-size` | ilk satırında backend'in adı bulunan yanıtın boyutu |
| `error-rate` | 500 ile cevaplanan isteklerin payı |
| `seed` | gecikme ve hata dizisini tekrarlanabilir kılar |

Gecikme yalnızca istek bir işçi tutarken harcanır; böylece kapasitesi düşük bir backend, gerçek bir
sunucu gibi, yük altında kuyruğu büyüdükçe yavaşlar ve `/healthz`'i kuyruk yarıdan fazla doluyken
503 döner. Her backend kendini ayrıca bir `X-Backend` başlığında da adlandırır.

| Backend'ler | Profil | Gecikme (medyan, p99) | Kapasite, kuyruk | Yanıt |
| --- | --- | --- | --- | --- |
| 1–6 | hızlı | 15 ms, 80 ms | 64, 128 | 4 KiB |
| 7–8 | orta | 30 ms, 200 ms | 32, 64 | 4 KiB |
| 9 | yavaş | 80 ms, 500 ms | 16, 32 | 4 KiB |
| 10 | büyük yanıt | 15 ms, 80 ms | 64, 128 | 256 KiB |

Entegrasyon testleri mock'u kullanmaz. Backend'lerini Go içinde kendileri kurar; orada bir test bir
backend'i yavaşlatabilir, hata verdirebilir ya da öldürebilir.

---

## 10. Değerlendirme

### 10.1 Yöntem

| | |
| --- | --- |
| Makine | Apple silicon, 10 çekirdek, macOS |
| Yük dengeleyici | tek süreç, `GOMAXPROCS` varsayılan |
| Backend'ler | tek süreçte on basit HTTP sunucusu, on baytlık gövde |
| Yapılandırma | round robin, iki retry'lı `retry_next_backend`, hız sınırlama kapalı, `max_connections: 10000` |
| Süre | seviye başına on saniye, bağlantılar boyunca açık tutuldu |

Yük üreteci, backend'ler ve yük dengeleyici aynı makineyi paylaşır; bu yüzden mutlak sayılar bir
alt sınırdır. Anlamı çalıştırmalar arası karşılaştırmalar taşır, çünkü her çalıştırma aynı dezavantajı
paylaşır. Referans ölçümleri `ab` üretti, ancak `ab` tek thread'lidir ve bin bağlantıda
başarısız oldu (`apr_socket_recv: Operation timed out`). Bunun üzerinde amaca özel bir sürücü
kullanıldı — bağlantı başına bir goroutine, keep-alive, istek başına gecikme kaydı. Sayıların üreteci
değil yük dengeleyiciyi anlattığını doğrulamak için sürücü önce doğrudan bir backend'e karşı
saniyede 108.000 istekle denendi.

v1.3.0'dan sonra yapılan ikinci bir kampanya, yük dengeleyicinin kendi maliyetini değil üç
algoritmayı karşılaştırmak için aynı ölçümleri profilli mock backend'lere (§9.4) karşı tekrarladı.
Yöntemi ve sonuçları §10.8'dedir; üreticisi `cmd/loadgen` repodadır, yani yeniden çalıştırılabilir.

### 10.2 Darboğazlar

İlk çalıştırma backend'lerin tek başına verdiğinden çok daha kötüydü: 100 bağlantıda saniyede
5.888 istek, p99 261 ms ve %6 hata; 1.000 bağlantıda her istek başarısız oldu.

**Backend bağlantıları yeniden kullanılmıyordu.** CPU profili zamanın %93'ünü sistem çağrılarında,
%2'sini yük dengeleyicinin kendi mantığında gösterdi; log `connect: can't assign requested
address` diyordu ve binin üzerinde soket `TIME_WAIT`'teydi. Go'nun varsayılan transport'u host başına
iki boşta bağlantı tutar; yük altında neredeyse her istek yeni bir bağlantı açtı ve geçici port
aralığı tükendi. Boşta bağlantı havuzu artık yapılandırmadan boyutlandırılır: toplamda
`max_connections`, backend'lere en az 32'şer olmak üzere bölünür.

**Sağlık kontrolü havuzu boşaltıyordu.** Bağlantılar yeniden kullanılınca doygunluk probe'ları zaman
aşımına uğrattı ve tüm backend'ler aynı anda çıkarıldı (§6.2). Denenmemiş havuza geri düşüş bunu
çözdü.

**İstek başına 32 KB bellek ayırma.** Heap profili tüm bellek ayırmanın %80,8'ini `ReverseProxy`'nin
kopyalama tamponuna bağladı: yirmi saniyelik bir çalıştırmada 56 GB, canlı heap ise yalnızca 22 MB
— bir sızıntı değil, çöp toplayıcı üzerinde baskı. `sync.Pool` destekli bir tampon havuzu bunu
kaldırdı.

| Değişiklik | Bağlantı | Önce | Sonra |
| --- | --- | --- | --- |
| Yeniden kullanılan backend bağlantıları | 100 | 5.888 istek/sn, p99 261 ms, %6 hata | 28.431 istek/sn, p99 10 ms, hatasız |
| Yeniden kullanılan backend bağlantıları | 1.000 | her istek başarısız | 35.075 istek/sn |
| Tampon havuzu | 500 | 32.147 istek/sn, p99 41,4 ms | 41.829 istek/sn, p99 32,4 ms |

### 10.3 Sonuçlar

| Bağlantı | Throughput | p50 | p95 | p99 | max |
| --- | --- | --- | --- | --- | --- |
| 100 | 40.616 istek/sn | 2,1 ms | 5,3 ms | 7,7 ms | 26,7 ms |
| 500 | 41.829 istek/sn | 11,2 ms | 24,0 ms | 32,4 ms | 147,5 ms |
| 1.000 | 41.208 istek/sn | 23,6 ms | 46,7 ms | 60,2 ms | 122,5 ms |
| 2.000 | 41.421 istek/sn | 47,3 ms | 87,5 ms | 106,6 ms | 712,6 ms |

Hiçbir seviyede istek başarısız olmadı. Throughput 100 bağlantıdan itibaren, paylaşılan makinenin
doyduğu yaklaşık saniyede 41.000 istekte düzdür; ardından gecikme, doymuş bir sunucu için Little
yasasının öngördüğü gibi eşzamanlılıkla orantılı büyür [12]. Başlangıca göre 100 bağlantıda
throughput yedi katına çıktı ve p99 261 ms'den 7,7 ms'ye düştü.

Özgün tasarım bin bağlantıda p95'i "tek haneli ya da düşük onlu milisaniyeler" olarak koymuştu.
Üreteci ve on backend'in tamamını da çalıştıran bir makinede ölçüm 46,7 ms'dir; 100 bağlantıda p95
5,3 ms'dir. Hedefi tek bir sayı olarak değil, beklenen yük cinsinden yeniden ifade ediyoruz (Ek B,
10. madde). Kalan profil sistem çağrılarının hakimiyetindedir; bu, çoğunlukla bayt taşıyan bir proxy
için beklenen şekildir. Daha fazla kazanç yük dengeleyicinin kodundan değil, işletim sisteminden ve ağ
yığınından gelir.

### 10.4 Mikro benchmark'lar

| Benchmark | İşlem başına süre | Bellek ayırma |
| --- | --- | --- |
| Round robin, 10 ve 100 backend | 1,9 ns, 1,8 ns | yok |
| Least connections, 10, 100 ve 1.000 backend | her boyutta 14 ns | yok |
| Weighted round robin, 10 ve 100 backend | 177 ns, 1,95 µs | yok |
| 10 goroutine ile RR, LC, WRR | 36 ns, 2,8 ns, 274 ns | yok |
| Sağlık sorgusu, tek başına ve raporlarla birlikte | 7,6 ns, 35 ns | yok |
| Hız sınırlayıcı, tek istemci ve çok istemci | 12 ns, 102 ns | yok |
| Retry budget, tek başına ve 10 goroutine ile | 3,5 ns, 144 ns | yok |
| Tek bir isteği iletmek, sıralı ve paralel | 32 µs, 11 µs | 13 KB, 104 |

Seçim, iletmenin yanında ucuzdur: en yavaş durum olan yüz backend üzerinde weighted round robin,
tek bir isteği iletmenin maliyetinin yaklaşık %6'sıdır. Weighted round robin havuzla doğrusal
büyür; round robin ve least connections büyümez. Round robin tek başına en hızlı stratejidir ama on
goroutine ile yirmi kat yavaşlar, çünkü hepsi tek bir sayacı artırır ve onu tutan cache line
çekirdekler arasında gidip gelir. İstek yolunda iletmenin kendisi dışında hiçbir şey bellek ayırmaz.

### 10.5 Regresyon korumaları

Yük testi tüm ortama ve sessiz bir makineye ihtiyaç duyar; bu yüzden her değişiklikte çalışamaz.
Zararsız görünen bir düzenlemenin geri alabileceği iki düzeltmenin her biri, olağan test paketinde
deterministik bir teste sahiptir:

| Koruma | Ölçtüğü | Sağlıklı | Düzeltme geri alınınca |
| --- | --- | --- | --- |
| Bağlantı yeniden kullanımı | 24 eşzamanlı isteklik 5 turun açtığı backend bağlantıları (sınır 48) | 24 | 112, başarısız |
| Bellek ayırma bütçesi | iletilen istek başına ayrılan bayt (sınır 32 KB) | yaklaşık 13 KB | yaklaşık 46 KB, başarısız |

İkisi de özgün hata geri getirilerek doğrulandı. İkincisi bellek ayırma sayısına değil bayta göre
karar verir, çünkü sayı her iki durumda da 104'tür: eksik havuz ayırmaların sayısını değil,
birinin boyutunu değiştirir.

### 10.6 Bir retry fırtınası altında retry budget

Her biri 100 ms sonra 500 döndüren dört backend, 50 eşzamanlı istek, istek başına en fazla üç retry,
`retry_on_5xx` açık:

| Budget | Backend'lere ulaşan deneme |
| --- | --- |
| Yükün tamamı (%100) | tam olarak 200 — her istek dört kez denendi; 150 retry gönderildi |
| Varsayılan (%20, bu testte alt sınır 1) | 10 çalıştırmanın her birinde ve iki CPU'da 20 çalıştırmanın daha her birinde 62 |

Retry'lar hatalı havuz üzerindeki yükü budget olmadan dört katına, budget ile 1,24 katına çıkardı.
Budget tek başına 3,5 ns, on goroutine yarışırken 144 ns tutar — paralel olarak bir isteği iletmenin
yaklaşık %1'i — ve iletmenin kendisi 104 bellek ayırma ve yaklaşık 13 KB'ta değişmeden kaldı.

### 10.7 Taramaya karşı power of two choices

Değişiklikten önce konan her ölçüt (§5.4), yerini aldığı tam taramaya karşı, §10.1'de anlatılan
makinede ölçüldü.

| | Hipotez | Tarama | Power of two choices | Karşılandı |
| --- | --- | --- | --- | --- |
| E1 | Seçim maliyeti artık havuzla büyümüyor | 10, 100, 1.000 backend'de 3,9, 47, 484 ns | üçünde de 14 ns; bellek ayırma yok | evet |
| E2 | İlk backend yanlılığı ortadan kalktı | 100 ardışık isteğin 100'ü `backend-1`'e | 20 çalıştırmada bir backend'de en fazla 18; 10.000 boşta seçimin hepsi eşit payın %20'si içinde | evet |
| E3 | Yavaş backend'lerden hâlâ kaçınılıyor | sürekli yük: 40 ms'lik backend'e 200'de 7 | 200'de 6–11 | evet, sürekli yük altında |
| E4 | Eşzamanlı gelen istekler dağılıyor | hiçbiri alınmadan okunan 50'lik bir burst: 50'si bir backend'e | bir backend'de en fazla 8 | evet |
| E5 | Yeniden üretilebilirlik korunuyor | — | tohumlanmış testler çalıştırmalar arasında aynı seçimleri verir | evet |

**Maliyet.** On backend'de tarama daha hızlıdır, 14 ns'ye karşı 3,9 ns, çünkü iki rastgele çekiliş
on atomik okumadan pahalıdır; on goroutine aynı anda seçim yaparken 2,8 ns'ye karşı 1,3 ns'dir. İki
fark da tek bir isteği iletmenin yaklaşık %0,03'üdür (§10.4). Yaklaşık otuz backend'den itibaren
power of two choices daha ucuzdur, bin backend'de ise 34 kat daha ucuzdur.

**Tek bir burst.** E3 önce, 60 isteğin bir yavaş ve iki hızlı backend üzerine aynı anda geldiği
mevcut testle ölçüldü. Orada yavaş backend 20 çalıştırmada 60 isteğin 16 ile 20'sini aldı, tarama
ise 16 — en kötü durumda üçte bir, yani eşit bir pay. Bir burst'teki seçimlerin çoğu tüm sayaçları
sıfır görür, bu yüzden iki strateji de neredeyse körlemesine seçer; taramanın 16'sı, o testte yavaş
olan ilk backend'e doğru yanlılığından geliyordu. Sürekli trafikte istekler yavaş backend'de birikir
ve o andan itibaren onu içeren her çifti diğer backend kazanır. Dolayısıyla ölçüt sürekli yük altında
tutar; tek bir burst ise hiçbir stratejinin eşit dağılımdan daha iyisini yapamadığı durumdur. Sürekli
durum için entegrasyon paketine bir test eklendi.

---

### 10.8 Tek tip olmayan bir havuzda algoritmalar

Yukarıdaki ölçümler on baytı mikrosaniyede döndüren on özdeş backend kullandı. Bu, yük
dengeleyicinin kendi darboğazlarını bulmak için doğru, stratejileri karşılaştırmak için yanlış bir
kurgudur: hiçbir zaman uçuşta istek olmaz, dolayısıyla least connections'ın okuyacağı bir sinyal
yoktur ve her backend eşit olduğu için ağırlıklar ile kapasite farkları hiç sınanmaz. Bu kampanya,
Compose içindeki sürüm imajını §9.4'teki on profilli mock backend'e karşı, makinede çalışan
`cmd/loadgen` ile sürüyor: bağlantı başına bir goroutine, keep-alive, kapalı döngü, atılan beş
saniyelik ısınma ve ölçülen yirmi saniye.

Havuzu yük dengeleyiciden önce profiller sınırlıyor. On backend aynı anda 528 isteğe hizmet ediyor
ve 1.056 isteği kuyruğa alıyor; bunun ötesinde bir backend hemen 503 döndürüyor. Yavaş backend —
kapasite 16, medyan 80 ms — bu kapasitenin %3,0'ını ve saniyede yaklaşık 200 isteği temsil ediyor.

| Bağlantı | Algoritma | Cevaplanan | p95 | p99 | Reddedilen | Yavaş backend'in payı |
| --- | --- | --- | --- | --- | --- | --- |
| 100 | round robin | 2.141/s | 253,0 ms | 397,9 ms | %3,6 | %10,0 |
| 100 | weighted round robin | 3.660/s | 71,7 ms | 150,6 ms | yok | %3,0 |
| 100 | least connections | 3.677/s | 71,2 ms | 151,0 ms | yok | %2,8 |
| 300 | round robin | 5.523/s | 107,0 ms | 334,3 ms | %7,6 | %10,0 |
| 300 | weighted round robin | 5.082/s | 126,5 ms | 325,4 ms | %0,3 | %3,0 |
| 300 | least connections | 5.352/s | 114,2 ms | 177,4 ms | yok | %2,6 |
| 2.000 | round robin | 3.518/s | 2,248 s | 2,930 s | %6,4 | %10,0 |
| 2.000 | weighted round robin | 3.292/s | 2,691 s | 3,586 s | yok | %3,0 |
| 2.000 | least connections | 3.266/s | 2,661 s | 3,251 s | %6,0 | %10,1 |

Her değer üç koşunun medyanıdır; 50'den 2.000 bağlantıya kadar altı seviye ölçüldü ve elli dört
koşunun tamamı performans raporundadır. Algoritmalar her seviyede araya alınarak koşuyor ve sıraları
her tekrarda döndürülüyor; çünkü onları blok blok koşan ilk deneme boyunca throughput istikrarlı
biçimde düştü ve bu, ilk ölçülen algoritmaya haksız avantaj verecekti. Beş sonuç önemlidir.

**Eşit olmayan bir havuzda eşit pay, havuzu en zayıf üyesiyle sınırlar.** Round robin'in dağılımı
her seviyede backend başına %10,0, üç haneye kadar: strateji tam olarak vaat ettiğini yapıyor. Ama
yavaş backend havuzun %3,0'ıdır; bu yüzden 100 bağlantıdan itibaren kapasitesini aşıp reddetmeye
başlıyor ve round robin'in bütün 503'leri ondan geliyor — 100 bağlantıda isteklerin %3,6'sı, üstündeki
her seviyede %6,4 ile %7,6 arası. Bu, gerçekleştirim hakkında değil, strateji seçimi hakkında bir
sonuçtur.

**Ağırlıklar ile least connections aynı dağılıma farklı yollardan varıyor.** Weighted round robin
yavaş backend'e, yapılandırıldığı kapasiteye oranlı ağırlığın karşılığı olan %3,0'ı verdi. Least
connections ise kapasite hakkında hiçbir şey bilmeden %2,6-3,1 aralığına vardı: yavaş bir backend
isteklerini daha uzun tuttuğu için daha meşgul görünür ve daha az seçilir. Havuzun kapasitesinin
altında bu, 50 bağlantıda %41, 100 bağlantıda %72 daha fazla cevaplanan istek demek — 2.141/s'ye
karşı 3.677/s — üstelik %3,6'ya karşı sıfır ret ve 253 ms'ye karşı 71 ms p95 ile.

**Dizde üçü throughput'ta yakınlaşır, geri kalan her şeyde ayrışır.** Havuzun 528 eşzamanlı isteğine
yakın olan 300 bağlantıda üçü birbirinin %8'i içinde; ama round robin isteklerin %7,6'sını reddediyor
ve p99'u least connections'ın 177 ms'sine karşı 334 ms. Dizin üstünde round robin diğerlerinden bir
miktar fazla cevaplıyor, karşılığında %6,4-6,8 reddediyor: aşırı yüklü bir mock mikrosaniyede
reddettiği için yükü atmak neredeyse bedavadır ve bağlantıyı yeni bir istek için serbest bırakır.
Cevapları retlerden ayırmayan bir throughput sayısı tam olarak bunu ödüllendirir.

**Least connections derin aşırı yükte sinyalini yitirir.** Yavaş backend'e verdiği pay 300
bağlantıda %2,6, 1.000'de %5,3 ve 2.000'de %10,1 — yani artık round robin'den iyi değil ve %6,0 ret
veriyor. Sebep aynı ani rettir: kuyruğu dolan backend hemen 503 döndürür, uçuştaki istek sayısı
sıfıra iner ve havuzun en boş backend'i haline gelir. Uçuştaki istekleri saymak doluluğu ölçer ve ani
bir ret, boşta olmaktan ayırt edilemez (§12).

**Dizin üstünde en pahalı bileşen yük dengeleyicinin kendisidir.** Her pencere boyunca örneklenen
`docker stats`, 300 bağlantıda yük dengeleyiciyi bir çekirdeğin %138'inde, on backend'in toplamını
%137'sinde gösteriyor; 2.000 bağlantıda %165-187'ye karşı %116-125. İstemcinin o noktada gördüğü şey
büyük ölçüde yük dengeleyicinin önündeki kuyruktur: yük dengeleyicinin kendi histogramı, istemci
tarafında p95'i 2,9 s olan trafik için 241 ms bildiriyor, istemcinin dağılımı iki tepeli (50 ms ve
2-5 s) ve saniyelerce süren bekleme 4 KiB dönen her backend'de aynı görünüyor — bu, seçimin
arkasındaki değil önündeki bir kuyruğun şeklidir. Dolayısıyla o seviyedeki gecikme, yük
dengeleyiciyi değil makineyi anlatır.

**Yük dengeleyicinin kendisi ne yaptı.** On sekiz koşunun tamamında `lb_rejected_requests_total` ve
`lb_retries_total` hiç hareket etmedi: istemcinin gördüğü her 503 bir backend'den geldi ve
`retry_on_5xx` kapalı olduğu için yük dengeleyici bu yanıtları olduğu gibi iletti. Üretici her iki
sayacı da ölçüm penceresinin iki ucunda okuyor, yani bu varsayım değil ölçümdür.

**Bir backend'i kaybetmek tek haneli sayıda retry'a mal oluyor.** 600 bağlantıda algoritma başına
bir koşuda, trafiğin %12'sini taşıyan hızlı bir backend ölçümün onuncu saniyesinde devre dışı
bırakıldı: hem mock'un düzgün boşalttığı SIGTERM ile hem de açık her bağlantıyı koparan SIGKILL ile.
Çökme bile yaklaşık 100.000 isteğin içinde yedi ile on retry'a mal oldu ve weighted round robin ile
least connections altında istemci hiçbir hata görmedi. Bunu ucuz kılan şey pasif sağlık kontrolüdür:
üst üste üç başarısız deneme backend'i havuzdan çıkarır ve bu hızlarda üç hata milisaniyeler sürer;
aktif probe'un fark etmesi altı saniyeye kadar sürebilirdi.

---

## 11. Tartışma

### 11.1 Sürümden sonra bulunan kusurlar

Race detector, v1.0.0'dan sonra iki gizli data race buldu; ikisi de yalnızca CI'da ve ikisi de yeni
işten değil, planın günlerinden kalmaydı:

- 9. günde yazılmış bir test: mock backend handler'ı, sağlık kontrolünün probe'u aynı handler'a
  gelirken korumasız değişkenlere yazıyordu;
- 11. günde eklenen yenileme, `/status` ve gerçek trafik altında weighted round robin okurken, sağ
  kalan bir backend'in ağırlığını düz bir alan olarak yazıyordu. Ağırlık artık atomiktir ve yalnızca
  erişim fonksiyonları üzerinden ulaşılabilir; böylece senkronize olmayan bir yazma artık derlenmez.
  Bir test, bir strateji seçim yaparken ağırlıkları değiştirir ve düz alan geri getirildiğinde
  `-race` altında başarısız olur.

İkisi de yerelde defalarca geçmişti. Ders race detector'ın güvenilmez olduğu değil, yalnızca
gerçekleşen iç içe geçmeleri bulduğudur: daha yavaş ve daha meşgul bir CI runner'ı, hızlı bir
geliştirme makinesinin üretmediği iç içe geçmeler üretti. Onları bulan, her push'ta tüm test
paketini `-race` altında çalıştırmaktı.

### 11.2 Tasarımın yanıldığı yerler ve bunun faydası

Sapmaların çoğu (Ek B) iyileştirmedir; üçü davranışı tasarımın öngörmediği biçimlerde değiştirdi.
Tamamen sağlıksız havuza isteği yine de sunmak (§6.2), bir yanıt zaman aşımı eklemek ve retry'ları
istekler genelinde sınırlamak (§6.5) aynı yerden gelir: tasarım tek bir isteğin başarısız olmasını
düşünmüştü, önemli olan ise pek çok isteğin birlikte başarısız olmasıydı. Aşırı yük, normal yükte
bağımsız olan istekleri — ortak probe'lar, ortak bağlantılar, ortak retry'lar üzerinden — birbirine
bağlar ve tasarımın istek başına bakışı bunu göremezdi. Her durumu ortaya çıkaran, yük altında
ölçmekti.

### 11.3 İyi ölçmek

Üç ölçüm dersi tekrar eder. Birincisi, bir benchmark kendi iskelesini ölçebilir: ilk hız sınırlayıcı
benchmark'ı, istemci adreslerini zamanlanan döngünün içinde kurduğu için sınırlayıcının ayırmadığı
16 baytı çağrı başına raporladı. İkincisi, bir koruma hatanın değiştirdiği şeyi ölçmelidir: bellek
ayırma sayısını saymak eksik tampon havuzunu yakalamazdı, bayt saymak yakalar. Üçüncüsü, teoride var
olan bir davranışın pratikte etki edecek bir şeyi olmayabilir: least connections demo ortamında
gösterilemedi, çünkü Little yasasına göre saniyede 20 istekte yaklaşık bir milisaniyede cevap veren
backend'lerin uçuşta neredeyse hiçbir şeyi olmaz; bilerek yavaşlatılmış backend'lerle gösterilmesi
gerekti.

### 11.4 Geçerliliğe yönelik tehditler

Yük testi, yük üreteci ve backend'lerle paylaşılan tek bir makinede yapıldı; bu yüzden mutlak
değerleri yük dengeleyicinin tek başına yapabileceğini olduğundan düşük gösterir. Karşılaştırmalar
sağlamdır, mutlak sayılar bir alt sınırdır. Backend'ler basit ve birbirinin aynıydı; bu, round robin'i
olduğundan iyi gösterir ve least connections'ın yardımcı olduğu durumları gizler. Retry fırtınası
deneyi dört backend ve tek bir hata biçimi kullandı; yavaş ve hızlı hataların farklı bir karışımı
farklı sayılar verirdi. Paylaşılan CI runner'larındaki benchmark zamanlamaları yüzde birkaç oynar;
bu yüzden yalnızca bayt ve bellek ayırma sayıları kapı olarak kullanılır.

---

## 12. Sınırlar ve gelecek çalışmalar

- **Burst altında least connections.** Çok sayıda istek aynı anda geldiğinde seçimlerin çoğu eşit
  sayılar görür; bu yüzden yavaş bir backend, üzerinde istekler birikene kadar eşit pay alır (§10.7).
- **TLS sonlandırma ve HTTP/2** kapsam dışında kalmaya devam ediyor; yük dengeleyici düz HTTP/1.1
  konuşur.
- **Backend başına uçuştaki istek sınırı yok.** İkinci kampanyanın bulduğu en belirgin eksik budur
  (§10.8). Least connections havuz derin aşırı yüke girene kadar kapasiteyi izler, sonra aşırı yükün
  yok ettiği bir sinyali izlemeye başlar: anında reddeden bir backend boşta görünür. Backend başına
  bir kabul sınırı, stratejinin göremediğini sınırlandırırdı.
- **Daha büyük bir havuz.** Ortamda on backend var. Otuz gibi daha büyük havuzlar için bir üreteç,
  power of two choices'ın taramayı geçtiği yerde ölçülmesini sağlar (§10.7).
- **Tek düğüm.** Hız sınırları, devre durumu ve retry budget süreç başınadır; birkaç yük dengeleyici
  örneğinin her biri kendi sınırını uygular.
- **Sticky session yok.** Backend'ler durumsuz olmalıdır.
- **Log toplama ve uyarılar** (Loki, Alertmanager) v1.0 sonrasına ertelenmişti ve hâlâ yok.
- **Gerçek bir dağıtım**, ücretsiz bir bulut katmanında ve herkese açık bir demoyla, hâlâ olası bir
  sonraki adımdır.

---

## 13. Sonuç

Ege-Balancer kendisi için konan hedefleri karşılar: üç strateji, sağlık kontrolü, yapılandırılabilir
hata stratejileri, bağlantı düşürmeden yenileme ve gözlemlenebilirlik; 22,6 MB'lık, root olmayan
bir imajda, kendi yüküyle paylaştığı bir makinede hiçbir isteği başarısız olmadan saniyede yaklaşık
41.000 istek karşılar. Daha kalıcı sonuç ise yöntemdir. Her karar verilmeden önce yazıya döküldü;
ölçülebildiği yerde ölçüldü; ölçümün tasarımla çeliştiği her yer kayda geçirildi, düzeltildi ve
mümkün olduğunda, düzeltme geri alınırsa başarısız olan bir teste dönüştürüldü. Üç darboğaz ve sürüm
sonrası üç ekleme spekülasyondan değil ölçümden geldi.

---

## Kaynakça

1. M. Mitzenmacher. *The Power of Two Choices in Randomized Load Balancing.* IEEE Transactions on
   Parallel and Distributed Systems, 12(10):1094–1104, 2001.
2. Y. Azar, A. Z. Broder, A. R. Karlin, E. Upfal. *Balanced Allocations.* SIAM Journal on
   Computing, 29(1):180–200, 1999.
3. M. Mitzenmacher. *How Useful Is Old Information?* IEEE Transactions on Parallel and Distributed
   Systems, 11(1):6–20, 2000.
4. B. Beyer, C. Jones, J. Petoff, N. R. Murphy (ed.). *Site Reliability Engineering: How Google
   Runs Production Systems.* O'Reilly, 2016. Bölüm 21 (Handling Overload) ve 22 (Addressing
   Cascading Failures).
5. Envoy Proxy belgeleri: yük dengeleyiciler (least request), panic threshold, circuit breaking ve
   retry budget. https://www.envoyproxy.io/docs/
6. Finagle belgeleri: retry budget. https://twitter.github.io/finagle/guide/
7. nginx belgeleri ve kaynak kodu, `ngx_http_upstream_round_robin.c` (smooth weighted round robin).
   https://nginx.org/en/docs/
8. HAProxy yapılandırma kılavuzu: `leastconn` dahil `balance` algoritmaları.
   https://docs.haproxy.org/
9. M. T. Nygard. *Release It! Design and Deploy Production-Ready Software*, 2. baskı. Pragmatic
   Bookshelf, 2018. Circuit breaker örüntüsü.
10. Go standart kütüphanesi: `net/http`, `net/http/httputil`, `sync`, `sync/atomic`, `math/rand/v2`;
    Go race detector; Go çöp toplayıcı rehberi (`GOMEMLIMIT`). https://pkg.go.dev/std,
    https://go.dev/doc/
11. J. Dean, L. A. Barroso. *The Tail at Scale.* Communications of the ACM, 56(2):74–80, 2013.
12. J. D. C. Little. *A Proof for the Queuing Formula: L = λW.* Operations Research, 9(3):383–387,
    1961.
13. R. Fielding, M. Nottingham, J. Reschke (ed.). *HTTP/1.1.* RFC 9112, 2022. §6.3, mesaj gövdesi
    uzunluğu.
14. `benchstat`, `golang.org/x/perf/cmd/benchstat`. https://pkg.go.dev/golang.org/x/perf/cmd/benchstat
15. Prometheus ve Grafana belgeleri. https://prometheus.io/docs/, https://grafana.com/docs/

---

## Ek A — Yapılandırma referansı

```yaml
listen_addr: ":8080"                # trafik; değiştirmek için yeniden başlatın
metrics_addr: ":8081"               # /metrics, /status, /healthz, /readyz, pprof; yeniden başlatma gerekir
enable_pprof: false                 # yeniden başlatma gerekir
algorithm: round_robin              # round_robin | least_connections | weighted_round_robin
failure_policy: retry_next_backend  # retry_next_backend | fail_fast | circuit_breaker
retry_on_5xx: false                 # 5xx yanıtı başarısız deneme say

retry:
  max_retries: 2                    # retry_next_backend altında en az 1
  budget_percent: 20                # uçuştaki retry: uçuştaki isteklerin en fazla bu payı
  min_retry_concurrency: 3          # ama her zaman en az bu kadar

circuit_breaker:                    # circuit_breaker altında zorunlu
  failure_threshold: 5
  open_duration: 30s

backends:
  - addr: "backend-1:5678"          # host:port, benzersiz
    weight: 1                       # yalnızca weighted round robin; yazılmazsa 1

health_check:
  path: "/healthz"
  interval: 5s
  timeout: 2s                       # interval'den kısa olmalı
  healthy_threshold: 2
  unhealthy_threshold: 3

timeouts:                           # yeniden başlatma gerekir
  connect_timeout: 2s
  response_timeout: 10s             # yazılmazsa read_timeout
  read_timeout: 10s
  write_timeout: 10s
  idle_timeout: 60s

limits:
  max_connections: 10000            # yeniden başlatma gerekir
  max_request_body_bytes: 10485760  # 10 MB
  rate_limit_per_ip: 100            # saniyede istek; 0 kapatır

logging:
  level: info                       # debug | info | warn | error
  format: json                      # json | text
```

`metrics_addr`, `response_timeout`, `budget_percent`, `min_retry_concurrency`, backend ağırlıkları
ve loglama için varsayılanlar uygulanır. Bilinmeyen alanlar hatadır. "Yeniden başlatma gerekir"
olarak işaretlenmeyen her şey `SIGHUP` ile uygulanır.

---

## Ek B — Özgün tasarımdan sapmalar

Bu ek, sistemin özgün tasarımdan (v1.6) ayrıldığı her yerin kaydıdır. Her madde v1.6'nın ne
dediğini, bunun yerine ne yapıldığını ve nedenini söyler; başlığın altındaki satır maddenin
dokunduğu v1.6 bölümünü, kararın ne zaman alındığını ve belgenin gövdesinde sonucun nerede
anlatıldığını verir. Her günün ayrıntısı geliştirme günlüğündedir.

### B.1 Commit'ler doğrudan main'e gider

*v1.6 §7.1, §7.3 ve §11'deki sürüm kontrolü ölçütü · 2. gün · §9.2*

**v1.6.** Korumalı bir `main`, `feature/<modül>` dalları ve CI'dan geçmiş bir pull request ile
birleştirilen her değişiklik.

**Bunun yerine.** Çalışma doğrudan `main`'e commit edilir ve CI pull request'lerde değil her
push'ta çalışır. Kalite kapısı değişmedi: gofmt, `go vet`, golangci-lint, govulncheck, derleme ve
bütün test paketi geçmeden hiçbir şey `main`'e ulaşmaz.

**Neden.** Tek geliştirici ve gözden geçiren kimse yokken dal töreni hiçbir şey kazandırmaz. Ekip
büyürse dal koruması ve pull request akışı yeniden açılmalıdır; hat, bir pull request'in ihtiyaç
duyacağı her kontrolü zaten çalıştırıyor.

### B.2 Weighted round robin deterministiktir

*v1.6 §5.3 ve planın 5. gün satırı · 5. gün · §5.2*

**v1.6.** Orantılı ve olasılıksal ağırlıklı seçim.

**Bunun yerine.** Smooth weighted round robin. Her seçim her backend'in kredisini ağırlığı kadar
artırır, en çok krediye sahip backend isteğe hizmet eder ve kredisi toplam ağırlık kadar düşer.

**Neden.** Yapılandırılan oranı yaklaşık değil tam tutturur, rastgelelik kaynağına ihtiyaç duymaz ve
ağır bir backend'in sıralarını yığmak yerine döngüye yayar: 5, 1, 1 ağırlıkları `a a b a c a a`
verir. Testler istatistiksel değil kesin olur — 1, 2 ve 3 ağırlıkları üzerinden 1.200 istek tam
olarak 200, 400 ve 600 üretir.

### B.3 Sağlık kontrolü arayüzü pasif yolu taşır

*v1.6 §6.2 · 6. gün, yenileme 11. günde eklendi · §4.2*

**v1.6.** `Start(ctx, backends)` ve `IsHealthy(addr)`'den oluşan örnek bir arayüz.

**Bunun yerine.** Arayüzde `ReportSuccess(addr)` ve `ReportFailure(addr)` da var; 11. günden beri
yapılandırma değişikliği için `Reload` da.

**Neden.** v1.6 §6.1 pasif sağlık kontrolü ister — gerçek trafikle beslenen ardışık hata sayacı — ve
proxy'nin, yazıldığı haliyle arayüz üzerinden onu besleyecek bir yolu yoktu. İki yol artık aynı
sayaçları hareket ettiriyor; gerçek trafikte başarısız olan bir backend bir sonraki probe'u
beklemeden çıkarılır. Yük altında çöken bir backend'de bunun neye değdiğini §10.8 ölçtü.

### B.4 Tamamen sağlıksız havuz yine de denenir

*v1.6 §5.5, bu durumu kapsamaz · 10. gün · §6.2*

**v1.6.** Bir backend başarısız olduğunda ne olacağı; ama sağlık kontrolü her backend'i aynı anda
sağlıksız işaretlediğinde ne yapılacağı değil.

**Bunun yerine.** Havuzda sağlıklı hiçbir şey yoksa istek yine de denenmemiş backend'lere sunulur ve
bu geri dönüş `no_healthy_backend` olarak sayılır — Envoy'un panic mode dediği şey. Bu yüzden her
backend sağlıksız ama hâlâ cevap veriyorsa istemci, yük dengeleyiciden 503 yerine backend'in kendi
yanıtını alır.

**Neden.** Doygunlukta sağlık probe'ları zaman aşımına uğrayan ilk istekler arasındadır. 10. günün
yük testinde her backend aynı anda sağlıksız işaretlendi ve yük dengeleyici 275.769 isteği reddetti;
yavaş bir sistem bozuk bir sisteme dönüştü. Hâlâ cevap verebilecek bir backend bir denemeye değer;
ölü olan bir başarısız denemeye mal olur ve istemci zaten alacağı 503'ü alır.

### B.5 Yapılandırma şemasına iki alan eklendi

*v1.6 §3.3 · 8. ve 10. gün · §7, Ek A*

**v1.6.** Gözlemlenebilirliğin nerede sunulacağı için bir ayar yok, profilleme için de yok.

**Bunun yerine.** `metrics_addr` (varsayılan `:8081`) `/metrics` ve `/status`'u, v1.2.0'dan beri de
`/healthz` ve `/readyz`'i sunar. `enable_pprof` (varsayılan `false`) bu porta Go'nun profilleme
uçlarını ekler.

**Neden.** Trafik portunda bu yollar hiçbir zaman bir backend'e iletilemezdi ve iç durum, yük
dengeleyiciye ulaşabilen herkese açık olurdu. Gözlemlenebilirlik sunucusunun bağlantı sınırı yoktur;
trafik portu doyduğu anda bile cevap vermeye devam eder. Profilleme heap ve goroutine durumunu açığa
verdiği için varsayılan olarak kapalıdır.

### B.6 Bir yanıt zaman aşımı eklendi

*v1.6 §3.3 · 11. gün · §6.3, Ek A*

**v1.6.** Bağlantı, okuma, yazma ve boşta zaman aşımları. Okuma ve yazma istemciyle konuşmayı,
bağlantı bir backend'e ulaşmayı sınırlar; bağlandıktan sonra bir backend'in cevap vermeye
başlamasının ne kadar sürebileceğini hiçbir şey sınırlamıyordu.

**Bunun yerine.** `timeouts.response_timeout`; verilmezse okuma zaman aşımını alır.

**Neden.** O olmadan, bağlantıyı kabul edip takılan bir backend isteği istemci tarafındaki yazma
zaman aşımı öldürene kadar tutar ve hata stratejisi başka bir backend denemeye hiç fırsat bulamaz.
Onunla takılan backend bırakılır ve istek başka yerde yeniden denenir — v1.6 §10.4'teki dayanıklılık
senaryosu.

### B.7 Grafana bir bellek bütçesine ihtiyaç duyar; yığın 5 sn'de bir scrape eder

*v1.6 §7.6 · 8. ve 12. gün · §8.3*

**v1.6.** İki izleme container'ı için de 256 MB bellek sınırı ve 15 saniyelik scrape aralığı.

**Bunun yerine.** Prometheus 256 MB'ta kalır. Grafana'nın sınırı 1 GB ve, daha önemlisi, 768 MiB'lık
bir `GOMEMLIMIT`'i var. Geliştirme yığını her 5 saniyede bir scrape eder ve yenilenir.

**Neden.** Grafana 13 boşta 256 MB'a sığıyor ama bir dashboard çizildiği anda öldürülüyordu
(`OOMKilled`); 512 MB bunu yalnızca erteledi. Sürekli yük altında belleği birinci dakikada
587 MiB'tan dördüncüde 751 MiB'a tırmandı ve artmaya devam etti, çünkü Go çalışma zamanı bütçesini
bilmediğinde geç toplar. Bütçe söylendiğinde aynı yük yaklaşık 774 MiB'ta sabitlendi ve beş dakika
boyunca yeniden başlamadı; sınırın geri kalanı heap dışındaki ayırmalar için paydır. Beş saniyede bir
scrape eden Prometheus 143 MiB kullanıyor. 5 saniyelik aralık, bir değişikliğin etkisini izlerken
dashboard'ları işe yarar kılan şeydir; üretim önerisi v1.6'nın yazdığı gibi kalır.

### B.8 Yük testi amaca özel bir üretici kullandı

*v1.6 §10.3 · 10. gün, v1.3.0'dan sonra düzeltildi · §10.1, §10.8*

**v1.6.** wrk ya da ab ile yük testi.

**Bunun yerine.** `ab` referans ölçümleri üretti, ama tek iş parçacıklıdır ve bin bağlantıda
`apr_socket_recv: Operation timed out` ile başarısız oldu. Bunun üstünde amaca özel bir sürücü
kullanıldı — bağlantı başına bir goroutine, keep-alive, kaydedilen gecikmelerden yüzdelikler — önce
doğrudan bir backend'e karşı saniyede 108.000 istekle sınandıktan sonra.

**Neden.** v1.6'nın sorduğu eşzamanlılığı yalnızca onu tutabilen bir sürücü ölçebilirdi. İlk sürücü
saklanmadı ve 10. günün sayıları doğrulanamaz kaldı; halefi `cmd/loadgen`, ölçüm yapılandırması ve
betiklerle birlikte repodadır ve ikinci kampanya (§10.8) onunla yapıldı.

### B.9 Bir demo konsolu eklendi

*v1.6'nın kapsamı dışında · v1.0.0'dan sonra · §4.2*

**v1.6.** Gösterimler için bir araç yok.

**Bunun yerine.** `cmd/demo`: yığını ayağa kaldıran ve normalde elle yazılacak eylemleri sunan yerel
bir konsol — sürekli trafik, dağılımı ölçmek, backend'leri durdurup başlatmak, algoritmayı
değiştirmek, hız sınırını göstermek ve her şeyi geri almak.

**Neden.** Bir gösterim, zaman baskısı altında uzun komutları doğru yazmaya bağlı olmamalı. Konsol
ürünün parçası değil bir geliştirme aracıdır: `docker`'ı çağırır, yapılandırma dosyasına yazar,
hiçbir sokette dinlemez, imajda yer almaz ve çalıştırdığı her komutu ekrana yazar; böylece olan
biteni gizleyen bir katman değil, yazmanın kısayolu olarak kalır.

### B.10 Gecikme hedefi yük cinsinden yeniden ifade edildi

*v1.6 §10.3 · 10. gün · §10.3, §10.8*

**v1.6.** Bin eşzamanlı bağlantıda tek haneli ile düşük onlu milisaniyeler arasında bir p95
gecikme.

**Bunun yerine.** Hedef yüz bağlantıda tutturuluyor (5,3 ms), binde kaçırılıyor (46,7 ms); yük
üreticisini ve on backend'i de çalıştıran bir makinede. Tek bir sayı olarak değil, gerçekte beklenen
yük cinsinden yeniden ifade edildi.

**Neden.** Test koşulları, yük dengeleyicinin makineye tek başına sahip olduğu bir dağıtımın
koşulları değil. İkinci kampanya bunu daha keskin gösterdi: yüksek eşzamanlılıkta istemcinin
gecikmesinin çoğu, yük dengeleyicinin handler'ından önce bağlantı kuyruğunda geçiyordu; yük
dengeleyicinin kendi p95'i ise bir mertebe düşük kaldı (§10.8).

### B.11 Bir retry budget eklendi

*v1.6'da yok; §5.5 yalnızca tek bir isteği sınırlar · v1.0.0'dan sonra · §6.5*

**v1.6.** `retry_next_backend` bir isteğin retry'larını `max_retries` ile sınırlar, fazlasını
değil.

**Bunun yerine.** Uçuştaki retry'lar uçuştaki isteklerin `retry.budget_percent`'i ile sınırlanır
(varsayılan 20), `retry.min_retry_concurrency` kadarına her zaman izin verilir (varsayılan 3);
reddedilen bir retry 503 ile cevaplanır ve `retry_budget_exhausted` olarak sayılır. İki alan da
isteğe bağlı ve varsayılanlıdır; mevcut bir yapılandırma geçerli kalır ve budget'ı kazanır.

**Neden.** Havuz hata vermeye başladığında her istek aynı anda retry eder ve backend'lere ulaşan
trafik, en az kaldırabilecekleri anda `1 + max_retries` katına kadar büyür. Tasarım Envoy'un retry
budget'ını izler. Dört hatalı backend ve elli eşzamanlı istekle backend'lere budget olmadan 200,
varsayılan budget'la 62 kez ulaşıldı.

### B.12 Least connections rastgele iki backend'i karşılaştırır

*v1.6 §5.2 · v1.0.0'dan sonra · §5.4, §10.7*

**v1.6.** Least connections en az aktif bağlantısı olan backend'i seçer; ilk gerçekleştirim bunu
havuzu tarayarak yapıyordu.

**Bunun yerine.** Rastgele iki farklı backend çekilir ve daha az meşgul olanı isteğe hizmet eder —
power of two choices, Envoy'un least request dengeleyicisindeki gibi. Yapılandırma adı
`least_connections` olarak kalır.

**Neden.** Tarama eşitlikleri havuz sırasına göre bozuyordu ve backend'ler istekler gelmeden cevap
verdiğinde eşitlik olağan durumdur: on backend üzerindeki 100 ardışık isteğin hepsi ilkine gitti,
birlikte gelen istekler de aynı backend'e yığıldı. İki rastgele örnek ikisini de kaldırır, havuz
boyutu ne olursa olsun 14 ns tutar (tarama bin backend'de 484 ns'ye çıkıyordu) ve sürekli yük
altında yavaş bir backend'den kaçınmayı korur — 200 isteğin 6 ile 11'i, taramada 7.

### B.13 Mock backend'ler projeye ait bir programdır

*v1.6 §8 · v1.1.0'dan sonra · §9.4*

**v1.6.** Her isteğe anında sabit bir metinle cevap veren on `hashicorp/http-echo` container'ı.

**Bunun yerine.** On backend'in hepsi olarak farklı profillerle çalışan tek bir program,
`cmd/mockbackend`: medyanı ve 99. yüzdeliğiyle belirlenen log-normal bir gecikme, ötesinde 503
döndüğü sınırlı kuyruklu bir kapasite, bir yanıt boyutu ve bir hata oranı. Altı backend hızlı, ikisi
daha yavaş ve daha az kapasiteli, biri zorlanan ve biri 256 KiB döndüren. Her biri adını hâlâ
söylüyor, artık bir `X-Backend` başlığında da.

**Neden.** Üç sonuç `http-echo`'nun on baytı mikrosaniyede döndürmesine bağlıydı: uçuşta hiçbir
zaman istek olmadığı için least connections gösterilemiyordu; yük testi iletimi on baytlık gövdeyle
ölçüyordu; her backend eşit olduğu için de ağırlıklar ve kapasite farkları hiç sınanmıyordu. Sağlık
kontrolü de yalnızca bir erişilebilirlik kontrolüydü; mock ise kuyruğu yarıdan fazla doluyken
kendini sağlıksız bildiriyor.

### B.14 Yük dengeleyici canlılık ve hazır olma sorgularına cevap verir

*v1.6 §7.6 · v1.1.0'dan sonra · §8.2*

**v1.6.** Metrikler ve loglar üzerinden izleme. 12. günün imajında container sağlık kontrolü yoktu,
çünkü distroless bir imajda onu çalıştıracak kabuk ya da `curl` yok.

**Bunun yerine.** Metrik portu, süreç cevap verdiği sürece 200 dönen `/healthz`'i ve en az bir
backend sağlıklıyken 200, hiçbiri değilse ya da kapanma başladıysa 503 dönen `/readyz`'i sunar;
kapanırken metrik portu, trafik portu boşalana kadar açık kalır. `lb -probe <url>` 200'de 0, aksi
halde 1 ile çıkar ve Compose onu `/healthz`'e karşı çalıştırır.

**Neden.** Bir container çalışma ortamı ya da orkestratör, ne `/metrics`'in ne de `/status`'un
cevapladığı evet-hayır bir soru sorar. Canlılık ile hazır olma ayrı tutulur, çünkü bütün
backend'leri düşmüş bir yük dengeleyici hâlâ çalışmaktadır ve onu yeniden başlatmak hiçbirini geri
getirmez. Probe bayrağı, imaja hiçbir şey eklemeden ona bir sağlık kontrolü kazandırır.

### B.15 Idempotent olmayan bir istek, yalnızca bir backend onu almadan önce retry edilir

*v1.6 §5.5 · v1.2.0'dan sonra · §6.3*

**v1.6.** `retry_next_backend`, istek ne olursa olsun başarısız bir denemeyi başka bir backend'de
yeniden dener.

**Bunun yerine.** Idempotent istekler (GET, HEAD, PUT, DELETE, OPTIONS, TRACE) her hatadan sonra
retry edilir. Diğerleri — POST, PATCH ya da yük dengeleyicinin tanımadığı bir metot — yalnızca
bağlantı hiç kurulamadığında retry edilir; hiçbir backend'in isteği görmediğini kanıtlayan tek hata
budur. Sonraki her hata isteği `not_retryable` olarak sayılan bir 503 ile bitirir.

**Neden.** Bir backend isteği yerine getirip yanıtı ulaşmadan hata verebilir ya da `retry_on_5xx`
altında 5xx dönebilir; yük dengeleyici ikisini de hiç ulaşmamış bir istekten ayırt edemez ve bir
POST'u yeniden denemek ikinci bir sipariş oluşturabilir. RFC 9110 aynı çizgiyi çeker; nginx ve Envoy
da böyle davranır. Kural yapılandırmada değil koddadır; proje güvensiz davranış için bir anahtar
sunmayı tutarlı biçimde reddetti.

### B.16 Her istek bir kimlik taşır

*v1.6 §7.6 · v1.2.0'dan sonra · §8.2*

**v1.6.** Metrikler ve yapılandırılmış loglar üzerinden gözlemlenebilirlik; yük dengeleyicinin
logundaki bir satırı bir backend'in logundaki aynı isteğe bağlayan hiçbir şey yok.

**Bunun yerine.** Her istek bir kimlik alır: istemcinin `X-Request-Id`'si en fazla 64 karakterlik
yazdırılabilir ASCII ise o, değilse `crypto/rand` ile üretilen bir değer. İstemciye döner, aynı
başlıkla backend'e iletilir ve istekle ilgili her log satırına `request_id` olarak yazılır.

**Neden.** On backend ve retry'larla başarısız bir istek birkaç logda görünür ve ortak bir anahtar
olmadan bunlar hizalanamaz. İstemcinin kendi kimliği, daha önce başlamış bir iz yük dengeleyicide
kopmasın diye korunur; her backend'in loguna ulaştığı için de sınırlanır ve denetlenir. Hız sınırlama
ve doğrulamadan önce atanır, böylece yük dengeleyicinin kendisinin reddettiği bir istek de
izlenebilir.

### Yapılmayan: epoll deneyi

*v1.6 §4.2 · §4.3*

v1.6'nın 10. ve 11. günlerin yanına planladığı isteğe bağlı öğrenme egzersizi yapılmadı; nedenleri
§4.3'tedir.

---

## Ek C — Sürüm geçmişi

| Sürüm | Tarih | Değişiklik |
| --- | --- | --- |
| 1.1 (v1.6) | 31 Ağustos 2026 | Gerçekleştirimden önce yazılan tasarım ve on iki günlük plan. Türkçe. [technical-design-v1.6-tr.md](technical-design-v1.6-tr.md) |
| 1.7 | 10 Eylül 2026 | v1.0.0 sonrası revizyon notları: v1.6'yı inşa edilen sisteme uyduran bölüm bölüm düzenlemeler. Yerini 1.8 aldı ve Eylül 2026'da repodan kaldırıldı; repo geçmişinde duruyor |
| 1.8 | 11 Eylül 2026 | İngilizce ve Türkçe tek bir tasarım makalesi olarak yeniden yazıldı: inşa edilen sistem, değerlendirme, sürüm sonrası benchmark'lar ve retry budget, ve v1.1.0 için gerçekleştirilip değerlendirilen power of two choices |
