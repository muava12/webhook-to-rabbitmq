
# Webhook Service (Gateway & Queue Manager)

**Webhook Service** adalah aplikasi penghubung (middleware) ringan berbasis **Go** yang berfungsi untuk menerima data webhook dari pihak ketiga (seperti WhatsApp gateway) dan meneruskannya ke antrian pesan (**RabbitMQ**) agar dapat diproses secara aman dan teratur oleh service lain.

## 🎯 Apa Fungsinya?

Fungsi utama dari service ini adalah untuk **menjamin data webhook tidak hilang** dan **mengatur beban trafik**.

Bayangkan jika ratusan pesan WhatsApp masuk secara bersamaan. Jika aplikasi utama langsung memproses semuanya, server bisa *down*. Service ini bertindak sebagai "resepsionis" yang mencatat pesan dan memasukkannya ke antrian, sehingga aplikasi utama bisa memprosesnya satu per satu sesuai kemampuannya.

### Fitur Unggulan:
1.  **Resilience (Ketahanan)**:
    *   Jika **RabbitMQ Down**: Service akan otomatis menyimpan data webhook di memori internal (RAM) sementara waktu.
    *   Saat RabbitMQ kembali hidup, data yang tertahan akan otomatis dikirim (flush).
2.  **Safety Buffer**: Mencegah data hilang ketika server backend sedang sibuk atau mati.
3.  **Monitoring & Notifikasi**: Terintegrasi dengan **ntfy.sh** untuk mengirim notifikasi jika:
    *   Koneksi RabbitMQ terputus/tersambung.
    *   Antrian memori penuh.
    *   Service baru dijalankan.
4.  **Optimasi Memori**: Dirancang agar ringan dan efisien menggunakan resource server.

---

## 🛠️ Daftar Service yang Didukung

Saat ini service dikonfigurasi untuk menerima webhook dari sumber berikut (hardcoded):
*   `molagis`
*   `muafa`
*   `kurir`

Setiap sumber akan memiliki routing key dan queue tersendiri di RabbitMQ (contoh: `wuzapi_molagis`).

## 🚀 Cara Menjalankan

### Menggunakan Docker Image dari GitHub (Recommended)

Image Docker sudah otomatis di-build dan tersedia di GitHub Container Registry.

```bash
# Pull image terbaru
docker pull ghcr.io/muava12/webhook-to-rabbitmq:latest

# Jalankan container
docker run -d \
  --name webhook-service \
  -p 8001:8001 \
  -e RABBITMQ_HOST=your-rabbitmq-host \
  -e RABBITMQ_USER=your-user \
  -e RABBITMQ_PASSWORD=your-password \
  ghcr.io/muava12/webhook-to-rabbitmq:latest
```


### Menggunakan Docker Compose

1.  Pastikan file `compose.yml` sudah dikonfigurasi dengan benar.
2.  Update image name di `compose.yml` ke:
    ```yaml
    image: ghcr.io/muava12/webhook-to-rabbitmq:latest
    ```
3.  Jalankan:

```bash
docker-compose up -d
```

### Build Lokal (Development)

```bash
# Build image lokal
docker-compose up -d --build

# Atau jalankan langsung dengan Go
go run main.go
```


---

## ⚙️ Konfigurasi (Environment Variables)

Anda dapat mengubah pengaturan ini melalui file `compose.yml` atau environment variable sistem.

| Variable | Default | Deskripsi |
| :--- | :--- | :--- |
| `WEBHOOK_PORT` | `8001` | Port aplikasi berjalan. |
| `RABBITMQ_HOST` | `localhost` | Host RabbitMQ. |
| `RABBITMQ_PORT` | `5672` | Port RabbitMQ. |
| `RABBITMQ_USER` | `wuzapi` | Username RabbitMQ. |
| `RABBITMQ_PASSWORD` | `mantab` | Password RabbitMQ. |
| `RABBITMQ_VHOST` | `/` | Virtual Host RabbitMQ. |
| `EXCHANGE_NAME` | `wuzapi` | Nama Exchange di RabbitMQ. |
| `QUEUE_PREFIX` | `wuzapi_` | Prefix untuk nama queue (misal: `wuzapi_molagis`). |
| `ROUTING_PREFIX` | `wa` | Prefix untuk routing key (misal: `wa.molagis`). |
| `MESSAGE_TTL_MINUTES`| `4320` | Masa berlaku pesan di queue (default 3 hari). |
| `MAX_QUEUE_LENGTH` | `50000` | Maksimal jumlah pesan dalam queue. |
| `NTFY_URL` | *(URL default)* | URL ntfy.sh untuk notifikasi monitoring. |

---

## 🔌 API Endpoints

### 1. Webhook Receivers
Endpoint untuk menerima data. Method: **POST**.

*   `/webhook/molagis` -> Queue: `[PREFIX]molagis`
*   `/webhook/muafa` -> Queue: `[PREFIX]muafa`
*   `/webhook/kurir` -> Queue: `[PREFIX]kurir`

**Contoh Request:**
```bash
curl -X POST http://localhost:8001/webhook/molagis \
     -H "Content-Type: application/json" \
     -d '{"message": "Hello World", "sender": "62812345678"}'
```

### 2. Utility Endpoints
*   **GET** `/health`: Cek status server, koneksi RabbitMQ, dan konfigurasi.
*   **GET** `/test-notification`: Mengirim notifikasi test ke ntfy.sh.

---

## 📦 Struktur Folder

*   `main.go`: Kode sumber utama aplikasi.
*   `Dockerfile`: Konfigurasi image Docker.
*   `compose.yml`: Orkestrasi container untuk Docker Compose.