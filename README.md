# Cara Rebuild jika ada perubahan kode
```
# Stop container yang sedang berjalan
docker compose down

# Rebuild dan start ulang
docker compose up --build -d

# Atau build dan restart service tertentu saja
docker compose build webhook-service
docker compose up -d webhook-service
```