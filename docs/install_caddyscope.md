# Install Caddy + scopecache op een verse VPS

Op een verse Ubuntu/Debian-VPS is het twee regels:

```bash
wget https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh
sudo bash install_caddyscope.sh
```

Klaar — Caddy + scopecache draait, `/help` is getest, `wrk` is
geïnstalleerd. Daarna kan dezelfde gebruiker `run_benchmark.sh`
ophalen en zien wat de cache aankan.

## Knoppen om aan te draaien

Allemaal optioneel als je de standaarden wil aanpassen — gewoon vóór
`sudo bash` plakken:

```bash
# Andere poort
sudo PORT=8080 bash install_caddyscope.sh

# Specifieke versie pinnen (in plaats van laatste)
sudo VERSION=v0.8.18 bash install_caddyscope.sh

# Grotere capaciteit
sudo MAX_STORE_MB=1024 SCOPE_MAX_ITEMS=1000000 bash install_caddyscope.sh

# Combineren
sudo VERSION=v0.8.18 PORT=8080 MAX_STORE_MB=500 bash install_caddyscope.sh
```
