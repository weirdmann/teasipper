# Audyt współbieżności i wydajności

Zakres: repozytorium `github.com/weirdmann/teasipper`, commit bazowy
`a224cfad1a604da263b9de6a5a3ca8baff6a327b`. Sprawdzono całą bibliotekę:
przed zmianą składała się z `main.go`, bez testów i przykładów poza README.
Jedynym transportem był TCP wymuszony jako `tcp4`; po zmianie obsługiwane są
`tcp`, `tcp4` i `tcp6`. Nie ma parsera ani dekodera wiadomości: TCP pozostaje
strumieniem bajtów, dlatego fuzz test parsera nie ma tu zastosowania.

## Potwierdzone problemy i naprawy

| Scenariusz w kodzie bazowym | Skutek | Naprawa / regresja |
| --- | --- | --- |
| `accept`, nadajnik, callback rozłączenia i stop równolegle używają `peers` bez blokady | wyścig danych lub `concurrent map iteration and map write` | Sesja ma własną mapę połączeń chronioną mutexem; testy równoległych resetów i zamknięć. |
| `Peer.Receive` wkłada do kanału wycinki tego samego bufora 1024 B | odczyt kolejnego fragmentu nadpisuje poprzedni; tymczasowy test bazowy na `net.Pipe` dał `first="BBBB" second="BBBB"` po wysłaniu `AAAA`, `BBBB` | `Conn.Read` zapisuje wyłącznie do bufora wywołującego; brak asynchronicznej kolejki danych. |
| `Peer.Close` zamyka `recv_chan` przy aktywnym nadawcy i nie jest idempotentny | `send on closed channel` lub `close of closed channel`; panika wystąpiła przy sprzątaniu bazowego benchmarku | `Conn.Close` jest idempotentne; zamknięcie socketu przerywa I/O, bez zamykania kanału danych. |
| Anulowanie z otwartym kanałem nadawczym oraz pełny kanał odbiorczy | goroutines zostają zablokowane na `range` lub wysłaniu; stop nie kończy pracy | Nowe API nie tworzy goroutines fan-in/fan-out; I/O jest synchroniczne i przerywalne kontekstem lub resetem. |
| Kolejne `Listen`/`Dial` nadpisuje pola endpointu używane przez stare goroutines | stara sesja może zamknąć nowy listener albo przekazać dane do nowego kanału | Reset wymienia sesję po zamknięciu jej zasobów; każde `Conn` pozostaje związane ze swoją sesją. |
| `Dialer.Deadline.Add(5*time.Second)` bez przypisania | oczekiwany limit połączenia nie działa | `DialTimeout` i kontekst są przekazywane do `net.Dialer.DialContext`. |
| Błąd zapisu i częściowy `Write` są ignorowane | cicha utrata danych | `Conn.Write` ponawia częściowy zapis; zwraca liczbę bajtów i błąd, w tym `io.ErrShortWrite` przy braku postępu. |
| Po wygaśnięciu jednorazowego deadline'u `Receive` ponawia odczyt w pętli | możliwy intensywny spin CPU; inne błędy odczytu nie sprzątają połączenia | Deadline jest ustawiany na każde aktywne I/O i potem usuwany; błędy są zwracane wywołującemu, który zamyka `Conn`. |
| Powolny peer blokuje rozsyłanie do wszystkich | niekontrolowany backpressure dla całego serwera | Serwer oddaje zaakceptowane połączenia aplikacji; każda synchroniczna operacja zapisu blokuje tylko swojego wywołującego. |

## Kontrakt konfiguracji, resetu i współbieżności

- `NewEndpoint(Config)` kopiuje i waliduje ustawienia. `Mode` wybiera klienta lub
  serwer; `Address`, `Network`, `LocalAddress`, `DialTimeout`, `ReadTimeout`,
  `WriteTimeout`, `NoDelay` i `KeepAlivePeriod` są stałe w działającej sesji.
  `LocalAddress` przyjmuje wyłącznie
  numeryczny lokalny IP (lub pusty host) i port, aby jego rozwiązywanie nie
  opóźniało anulowania. `Config()` zwraca kopię.
- `Start(ctx)` uruchamia zatrzymany endpoint. Kontekst obejmuje zestawienie
  sesji, nie jej późniejszy czas życia. `Reset(ctx, nil)` zachowuje konfigurację;
  `Reset(ctx, &cfg)` najpierw waliduje nową, potem zamyka starą sesję i uruchamia
  nową. Nieprawidłowa konfiguracja pozostawia starą sesję aktywną. Błąd
  ponownego uruchomienia pozostawia endpoint zatrzymany z wybraną konfiguracją.
- Reset zamyka stary listener i wszystkie zarządzane połączenia, przerywa
  blokujące się na nich I/O i nie przenosi oczekujących danych. Zwraca sukces,
  gdy nowy serwer jest zbindowany albo klient połączony. Stare `Conn` nigdy nie
  staje się połączeniem nowej sesji; już ukończone bajty mogą nadal być
  przetworzone przez aplikację. Klient wymaga jawnego `Reset` po zdalnym
  rozłączeniu.
- `Close` kończy endpoint na stałe. Równoległe `Close` czekają na ukończenie
  zamykania. `Start` i `Reset` są serializowane; kolejne `Reset` przerywa
  trwające zestawianie połączenia. `Close` też przerywa setup. `Accept` oraz
  `ReadContext`/`WriteContext` można anulować również podczas oczekiwania za
  inną operacją tego samego kierunku. Anulowanie pojedynczego I/O nie zamyka
  połączenia.
- Konfigurowalne `ReadTimeout`/`WriteTimeout` obejmują aktywny odczyt lub zapis
  po uzyskaniu bramki. Kontekst obejmuje również oczekiwanie w kolejce.
  Ręczne `SetReadDeadline`/`SetWriteDeadline` utrzymują absolutne deadline'y
  przez kolejne wywołania i mogą przerwać blokujące I/O; `Conn` implementuje
  `net.Conn`. Najwcześniejszy z ręcznego deadline'u, timeoutu operacji i
  deadline'u kontekstu obowiązuje bieżące wywołanie.
  Własność `[]byte` pozostaje u wywołującego; musi on utrzymać bufor do końca
  synchronicznego wywołania. Połączenia zaakceptowane przez serwer trzeba
  zamknąć, gdy aplikacja kończy ich używanie. Nie ma wewnętrznej puli buforów,
  która mogłaby zatrzymywać duże bloki pamięci.

README zawiera pełny przykład i tabelę migracji z poprzedniego API kanałowego.

## Pomiary

Środowisko: Go 1.26.3, Windows/amd64 (build 10.0.26200), Intel Core Ultra 7
265H, 16 logicznych CPU, domyślne `GOMAXPROCS`. Pomiar na TCP loopback
`127.0.0.1`, 5 próbek po 1 s, mediana; każda iteracja zapisuje i odczytuje
64 lub 4096 bajtów. Setup połączeń jest poza timerem. MB/s to metryka
`testing.B.SetBytes`. Bazowy harness znajduje się w
`benchmarks/legacy_benchmark_test.go.txt`; mierzy taki sam transfer bajtów,
ale nie weryfikuje zawartości każdej próbki i nie sprząta goroutines, ponieważ
anulowanie powodowało panikę w starym kodzie. Bazowe `0 allocs/op` obejmuje
wadliwy odbiór przez alias współdzielonego bufora. Nowy benchmark weryfikuje
zawartość po rozgrzewce i sprząta wszystkie zasoby.

| Ścieżka | Przed: ns/op | Po: ns/op | Przed: B/op | Po: B/op | Przed: allocs/op | Po: allocs/op | Przed: MB/s | Po: MB/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| TCP 64 B, domyślne timeouty | 20 302 | 9 195 | 0 | 0 | 0 | 0 | 3,15 | 6,96 |
| TCP 4096 B, domyślne timeouty | 31 606 | 13 841 | 0 | 0 | 0 | 0 | 129,60 | 295,94 |

Mediana czasu spadła odpowiednio o 54,7% i 56,2%, a przepustowość wzrosła
o 121,0% i 128,3%. Bazowe zakresy czasu to 18 999–25 608 ns/op (64 B) i
29 153–35 789 ns/op (4096 B); po zmianie 8 855–10 762 oraz
9 872–15 364 ns/op. Każda próbka i liczba iteracji znajduje się w
`benchmarks/before-transfer.txt` i `benchmarks/after-transfer.txt`. Wynik
podano jako medianę pięciu próbek, bez twierdzenia o istotności statystycznej;
`benchstat` nie był dostępny w środowisku. Brak alokacji w obu wersjach nie
oznacza równoważnego kontraktu: stary kod zwracał alias do zmienianego bufora.

Pomiary resetu i konfiguracji timeoutów (nowe operacje bez bazowego
odpowiednika):

| Ścieżka | ns/op | B/op | allocs/op | Uwagi |
| --- | ---: | ---: | ---: | --- |
| TCP 64 B, timeout 1 s na odczyt/zapis | 12 842 | 0 | 0 | Koszt deadline'ów podczas aktywnego I/O. |
| Lokalny reset serwera | 74 986 | 948 | 15 | Zamknięcie i ponowny bind listenera, bez klienta. |
| Reset klienta do gotowości | 229 638 | 3 152 | 43 | Ponowny dial, `Accept` i pierwszy bajt na loopback. |

Przy 64 B timeout zwiększa medianę czasu o 39,7% wobec wariantu bez
deadline'ów; koszt wynika z ustawiania i czyszczenia deadline'u dla każdego
I/O. Próby miały 11 332–38 023 ns/op, z wysoką zmiennością ostatniej
próbki. Lokalny reset serwera miał 68 876–78 734 ns/op, a gotowość klienta
213 331–246 102 ns/op (po 200 iteracji w pięciu próbkach). Surowe pomiary
są w `benchmarks/after-reset.txt`. Koszty alokacji resetu obejmują jednorazowe
utworzenie socketu, mapy i bramek nowej sesji; nie są kosztem ustalonego I/O.
Reset lokalny i gotowość klienta są rozdzielone, ponieważ czas ponownego
połączenia zależy od sieci i nie ma stałej granicy wyrażonej samym kosztem
lokalnego zamknięcia.

### Rozszerzenie dla sorter-gateway

Commit `b7f3dcd` dodał trwałe absolutne deadline'y `net.Conn` i opcje TCP
`NoDelay` oraz `KeepAlivePeriod`. Gateway ustawia deadline całej ramki, więc
timeout każdej pojedynczej operacji nie zastępuje tego kontraktu. Testy
potwierdzają przerwanie zablokowanego I/O, zachowanie ręcznego deadline'u po
anulowaniu kontekstu oraz konfigurację socketu po dial i accept.

Ponowny pomiar tej wersji w tym samym środowisku: Go 1.26.3, Windows/amd64,
Intel Core Ultra 7 265H, TCP loopback, 5 próbek po 1 s, bez detektora wyścigów.
Poniżej mediana; surowe próbki zapisano w
`benchmarks/after-gateway-extension.txt` i
`benchmarks/after-gateway-extension-timeout.txt`.

| Ścieżka | ns/op | B/op | allocs/op | MB/s |
| --- | ---: | ---: | ---: | ---: |
| TCP 64 B, bez timeoutu | 8 683 | 0 | 0 | 7,37 |
| TCP 4096 B, bez timeoutu | 10 410 | 0 | 0 | 393,49 |
| TCP 64 B, timeout 1 s na odczyt/zapis | 11 356 | 0 | 0 | 5,64 |

Zakresy czasu wyniosły odpowiednio 8 354–9 337, 9 594–14 028 i
10 687–12 551 ns/op. Ponowny pomiar ma zwykłą zmienność na współdzielonej
maszynie; nie przypisujemy różnicy względem wcześniejszej tabeli samemu
rozszerzeniu. Wszystkie ustalone ścieżki nadal wykazują 0 B/op i
0 allocs/op. `go test ./... -count=10`, `go vet ./...` oraz kompilacja testów
dla Linux przeszły. `CGO_ENABLED=1 go test -race ./... -count=10`
przeszedł w lokalnym WSL (Linux/amd64, Go 1.25.0, gcc 11.4.0).

## Odtworzenie

Polecenia uruchomić z katalogu repozytorium w PowerShell, z `go` w `PATH`.
Pomiar wydajności należy uruchomić bez `-race`, na nieobciążonej maszynie:

```powershell
go version
git rev-parse HEAD
go test ./... -count=3 -timeout=60s
go vet ./...
go test ./... -run '^(TestConn|TestEndpointConcurrentClose)' -count=30 -timeout=90s
go test -run '^$' -bench '^BenchmarkEndpointOneWay(64|4096)$' -benchtime=1s -count=5 -timeout=2m
go test -run '^$' -bench '^BenchmarkEndpointOneWay64Timeout1s$' -benchtime=1s -count=5 -timeout=2m
go test -run '^$' -bench '^BenchmarkEndpoint(ServerResetLocal|ClientResetReady)$' -benchtime=200x -count=5 -timeout=2m
```

Na Windows wielokrotne testy/resetowanie TCP mogą czasowo wyczerpać porty
efemeryczne przez `TIME_WAIT`; wtedy powtarzać pełny zestaw dopiero po
odnowieniu portów. Do odtworzenia bazowego benchmarku:

```powershell
git worktree add ..\teasipper-baseline a224cfad1a604da263b9de6a5a3ca8baff6a327b
Copy-Item .\benchmarks\legacy_benchmark_test.go.txt ..\teasipper-baseline\benchmark_test.go
Push-Location ..\teasipper-baseline
go mod download
go test -run '^$' -bench '^BenchmarkEndpoint' -benchtime=1s -count=5 -timeout=2m
Pop-Location
```

Przed zmianami `go test ./...` i `go vet ./...` przechodziły, ale repozytorium
nie miało żadnych testów. Próba `go test -race ./...` bez testów dawała pozorny
sukces (`[no test files]`). Po dodaniu wykonywalnych testów `go test -race`
nie może zbudować `runtime/cgo` na Windows: host nie ma kompilatora
`gcc`/`clang` (`cgo: C compiler "gcc" not found`). W lokalnym WSL z gcc
detektor wyścigów przeszedł 10 pełnych przebiegów na Go 1.25.0. Nie jest to
sprawdzenie wszystkich harmonogramów ani wszystkich platform; warto zachować
ten test w CI z kompilatorem C.

Po zmianach formatowanie, `go vet ./...`, pełne `go test ./... -count=3`
oraz 30 powtórzeń testów `net.Pipe`/fake i równoległego `Close` przeszły.
Próba 30 powtórzeń *całego* zestawu TCP po serii benchmarków kończyła się
systemowym `connectex` z powodu zajętych portów `TIME_WAIT`; pojedynczy
i potrójny przebieg po odnowieniu portów przeszedł. Nie klasyfikujemy
tymczasowego wyczerpania portów jako naprawionego błędu biblioteki.

Pozostałe ryzyka: audyt obejmuje lokalny TCP loopback i kontrolowane
`net.Pipe`, bez sieci zewnętrznej, IPv6 na każdym systemie operacyjnym i
długotrwałego testu obciążeniowego. Czasy resetu klienta na loopback nie
przewidują zachowania odległego hosta. W repozytorium nie ma kodu protokołu
aplikacyjnego do fuzzowania; aplikacja powinna dodać framing i osobne testy
jego parsera, jeśli go potrzebuje.
