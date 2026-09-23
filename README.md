# whitelist-bypass

Обход белых списков в домашнем k3s: StatefulSet, **один под = один человек**.
Манифесты — `home-flux/apps/home/whitelist-bypass/`. Здесь — код двух образов.

- `ghcr.io/ipqw/whitelist-bypass-pod` — под человека. Headless-creator'ы апстрима
  [kulikov0/whitelist-bypass](https://github.com/kulikov0/whitelist-bypass), GUI-Creator
  для входа в аккаунты и `wlb-agent`: держит по creator'у на каждую платформу, где есть
  вход, и отдаёт ссылки на комнаты по HTTP (`GET /rooms`, `POST /recreate/<p>`,
  `POST /notified/<p>`).
- `ghcr.io/ipqw/whitelist-bypass-bot` — один VK-бот на все поды. Карта «VK id → номер
  пода»; каждый видит и пересоздаёт только свои комнаты, новые ссылки приходят сами.

```
pod/Dockerfile   pod/wlb-login   agent/main.go
bot/Dockerfile   bot/main.go
.github/workflows/image.yml   # тег → оба образа в ghcr
```

## Переменные пода

`wlb-agent` читает только эти — остальное из докер-образа апстрима здесь не работает:

- `UPSTREAM_SOCKS` — SOCKS5 выхода трафика туннеля, `host:port`.
- `ALLOW_PRIVATE_DST` — `1`/`true`: creator пускает joiner'а на частные адреса
  (10/8, 172.16/12, 192.168/16, 100.64/10), например в wg-сеть. С `UPSTREAM_SOCKS`
  они открываются с той стороны прокси.
- `RESOURCES` — режим ресурсов creator'ов, по умолчанию `default`.

## Выпуск

Тег `v<апстрим>-<сборка>`, например `v0.4.4-1`. Обновить апстрим — в `pod/Dockerfile`
тег образа `whitelist-bypass-bot` и AppImage с его sha256.

## Обход новому человеку

1. `home-flux`: `replicas` +1 в `statefulset.yaml`, человек в `users.json` бота
   (`vk_id`, `name`, `pod` = номер нового пода).
2. Человек пишет сообществу бота любое сообщение — иначе VK не даст боту ему писать.
3. Вход в его аккаунты (один раз):
   ```
   kubectl -n whitelist-bypass exec whitelist-bypass-N -- wlb-login
   kubectl -n whitelist-bypass port-forward pod/whitelist-bypass-N 6080
   # http://localhost:6080/vnc.html → «+» → платформа → Create new → логин
   kubectl -n whitelist-bypass exec whitelist-bypass-N -- wlb-login stop
   ```
   После `stop` агент в течение 5 секунд поднимет creator'ы, бот пришлёт ссылки.
4. На телефоне — APK той же версии апстрима.

Один creator = один joiner: на одну ссылку — одно устройство.
