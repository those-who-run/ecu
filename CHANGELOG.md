# Changelog

## [0.1.16](https://github.com/those-who-run/ecu/compare/v0.1.15...v0.1.16) (2026-11-01)


### Features

* make ecu's race state public: race, track and pit ([#74](https://github.com/those-who-run/ecu/issues/74)) ([3117eb9](https://github.com/those-who-run/ecu/commit/3117eb9c6ebe7fef02008ce4b2d6918d77584b46))

## [0.1.15](https://github.com/those-who-run/ecu/compare/v0.1.14...v0.1.15) (2026-10-31)


### Features

* **race:** the ranked kind, qualifying or practice with the field out ([#70](https://github.com/those-who-run/ecu/issues/70)) ([0ea5b35](https://github.com/those-who-run/ecu/commit/0ea5b35327e24baf91f344a929d697aa723bc65e))


### Bug Fixes

* **race:** paused_ms counts the pauses before an ecu restart ([#73](https://github.com/those-who-run/ecu/issues/73)) ([27701e4](https://github.com/those-who-run/ecu/commit/27701e40148610891db2a96695646692982de2dd))
* **rules:** a ranked session's first rank, its finish's rank, and the pit lane ([#72](https://github.com/those-who-run/ecu/issues/72)) ([80bbdb4](https://github.com/those-who-run/ecu/commit/80bbdb4f04eb24c949a26fc9972fa1cb6c98d7a3))

## [0.1.14](https://github.com/those-who-run/ecu/compare/v0.1.13...v0.1.14) (2026-10-31)


### Features

* **race:** a grid behind the line is a grid start ([#69](https://github.com/those-who-run/ecu/issues/69)) ([babf1a9](https://github.com/those-who-run/ecu/commit/babf1a961fb9bea918b941090415b637d9f207f9))
* **replay:** --seed for reproducible ids, and one line per session at the end ([#64](https://github.com/those-who-run/ecu/issues/64)) ([0591323](https://github.com/those-who-run/ecu/commit/0591323ffabad4b31d681a7962bef501825fe81d))

## [0.1.13](https://github.com/those-who-run/ecu/compare/v0.1.12...v0.1.13) (2026-10-31)


### Bug Fixes

* **race:** the field gates read the session kind, not the cars-on-track flag ([#63](https://github.com/those-who-run/ecu/issues/63)) ([8b11ead](https://github.com/those-who-run/ecu/commit/8b11eadf9c331aff33fd7d5072a00f8757162117))
* **status:** live's status writer waits for NATS's first connection ([#62](https://github.com/those-who-run/ecu/issues/62)) ([3605ec0](https://github.com/those-who-run/ecu/commit/3605ec056e6a6142f826aeb73349c220e2dd844a))

## [0.1.12](https://github.com/those-who-run/ecu/compare/v0.1.11...v0.1.12) (2026-10-31)


### Features

* **rules:** no track at the start, the sure track in the race summary ([#60](https://github.com/those-who-run/ecu/issues/60)) ([4a72685](https://github.com/those-who-run/ecu/commit/4a726858e69ef4afe3919fb2bed47984c59d0060))


### Bug Fixes

* **race:** keep the track over an ecu restart on lap 1 ([#59](https://github.com/those-who-run/ecu/issues/59)) ([b66b596](https://github.com/those-who-run/ecu/commit/b66b5968ed8a12bab8cd1c0695b15d24086d522e))

## [0.1.11](https://github.com/those-who-run/ecu/compare/v0.1.10...v0.1.11) (2026-10-30)


### Features

* **race:** continue a session after an ecu restart, from the KV ([#56](https://github.com/those-who-run/ecu/issues/56)) ([3538326](https://github.com/those-who-run/ecu/commit/3538326567f7b10def876d9c4c14dbd18c03f91b))


### Bug Fixes

* **race:** the track detector sees the frames the machine ignores ([#57](https://github.com/those-who-run/ecu/issues/57)) ([2e1defe](https://github.com/those-who-run/ecu/commit/2e1defeb7a32527f17b16d9a4325b5ce6a702392))

## [0.1.10](https://github.com/those-who-run/ecu/compare/v0.1.9...v0.1.10) (2026-10-30)


### Features

* **ecu:** ecu tracks detect and ecu tracks learn, as v1's commands ([#54](https://github.com/those-who-run/ecu/issues/54)) ([9dca62e](https://github.com/those-who-run/ecu/commit/9dca62e1182f094a2d88b207cffdef4a31c77523))
* **race:** joined_on_lap on a session joined part way ([#50](https://github.com/those-who-run/ecu/issues/50)) ([d16a38f](https://github.com/those-who-run/ecu/commit/d16a38f80e931c29a68e8aede0a10a07648bac94))
* **race:** the running session summary, the car table and car_name ([#52](https://github.com/those-who-run/ecu/issues/52)) ([f6c93bd](https://github.com/those-who-run/ecu/commit/f6c93bddaaff8241209c4cc8d39115c3f555435f))
* **status:** the live KV and /healthz idle (ecu.status, ecu.session, ecu.pit, ecu.fuel) ([#53](https://github.com/those-who-run/ecu/issues/53)) ([859bc81](https://github.com/those-who-run/ecu/commit/859bc81b9d667106f1bbfe1c1412a3770fbd0600))


### Bug Fixes

* **track:** widen Fuji's box, and show a miss as none ([#51](https://github.com/those-who-run/ecu/issues/51)) ([ff8d51a](https://github.com/those-who-run/ecu/commit/ff8d51a9dfda960f583deb1fde3d5248d4e6f780))

## [0.1.9](https://github.com/those-who-run/ecu/compare/v0.1.8...v0.1.9) (2026-10-30)


### Features

* **race:** pauses, flagged and frozen (session.paused, session.resumed) ([#46](https://github.com/those-who-run/ecu/issues/46)) ([ae1f1c1](https://github.com/those-who-run/ecu/commit/ae1f1c19d45ab2394c8868b9765f6e935240add4))
* **race:** telemetry lost and restored, live only ([#44](https://github.com/those-who-run/ecu/issues/44)) ([abb5593](https://github.com/those-who-run/ecu/commit/abb5593d8501e9698a7d1391d3e57c2cc396dba3))
* **race:** the start check: a drive, a replay or a demo ([#47](https://github.com/those-who-run/ecu/issues/47)) ([d23280a](https://github.com/those-who-run/ecu/commit/d23280ab1e985b17c8affa55af9100c872f2e965))
* **rules:** the telemetry rule (telemetry) ([#48](https://github.com/those-who-run/ecu/issues/48)) ([153fee5](https://github.com/those-who-run/ecu/commit/153fee5d27a5be3c5f3e0b402192601da45ed1aa))
* **rules:** the temps rule and car.alarm ([#45](https://github.com/those-who-run/ecu/issues/45)) ([b7458ab](https://github.com/those-who-run/ecu/commit/b7458abd4f1ace4d991bda5daed5e431cf52f6bd))

## [0.1.8](https://github.com/those-who-run/ecu/compare/v0.1.7...v0.1.8) (2026-10-29)


### Features

* **rules:** race_summary, the summary at the finish and on leaving a solo session ([#41](https://github.com/those-who-run/ecu/issues/41)) ([66fa783](https://github.com/those-who-run/ecu/commit/66fa783955bb06c53c91a5f7a107401c9af98dfc))
* **rules:** the fuel rule and the fuel model ([#40](https://github.com/those-who-run/ecu/issues/40)) ([946abf5](https://github.com/those-who-run/ecu/commit/946abf5e5d9595f8fdac0640a41f1ae21eb8cbe5))
* **rules:** the lap_time rule ([#38](https://github.com/those-who-run/ecu/issues/38)) ([aa9dd5a](https://github.com/those-who-run/ecu/commit/aa9dd5a0d1f205ff9d8c2d6a949b51b307a338af))
* **rules:** the position rule (places gained and lost) ([#37](https://github.com/those-who-run/ecu/issues/37)) ([a4c190f](https://github.com/those-who-run/ecu/commit/a4c190f7fdc193de2accfc5c76580a2f6cdb5b3f))
* **rules:** the rule engine and callouts on the bus (final_lap) ([#42](https://github.com/those-who-run/ecu/issues/42)) ([bc600cb](https://github.com/those-who-run/ecu/commit/bc600cb26f299e8076044a7968286926e6b4f1ef))
* **rules:** the start brief (start_brief) ([#39](https://github.com/those-who-run/ecu/issues/39)) ([1922013](https://github.com/those-who-run/ecu/commit/19220138f9f155e0b3f4d7d21281b91bdc1de5ff))

## [0.1.7](https://github.com/those-who-run/ecu/compare/v0.1.6...v0.1.7) (2026-10-29)


### Features

* **race:** the final lap and the settled position (race.final_lap, race.position) ([#34](https://github.com/those-who-run/ecu/issues/34)) ([def6d2e](https://github.com/those-who-run/ecu/commit/def6d2ef11a7f353411f8a216ad22f354dcfacc9))
* **race:** the lap model: pit roles, which laps count, lap.best ([#35](https://github.com/those-who-run/ecu/issues/35)) ([06a8086](https://github.com/those-who-run/ecu/commit/06a8086a37624bb6ccda7f5e292588f193d0f3d5))

## [0.1.6](https://github.com/those-who-run/ecu/compare/v0.1.5...v0.1.6) (2026-10-29)


### Features

* **race:** publish an entry withdrawn (pit.teleport) ([#31](https://github.com/those-who-run/ecu/issues/31)) ([6b805a7](https://github.com/those-who-run/ecu/commit/6b805a7dfe3420b871495fa9b2b10999e8a85509))
* **race:** the finish, by flag and timed (race.finished) ([#32](https://github.com/those-who-run/ecu/issues/32)) ([c339670](https://github.com/those-who-run/ecu/commit/c3396706137b57ab75ff65eec8a6d3433dff8659))


### Dependencies

* contracts v0.2.3, with the 20 Hz frame catalogued ([#30](https://github.com/those-who-run/ecu/issues/30)) ([597c1f6](https://github.com/those-who-run/ecu/commit/597c1f6d2ea18afd810faf6dbfb3f683255d7da3))

## [0.1.5](https://github.com/those-who-run/ecu/compare/v0.1.4...v0.1.5) (2026-10-29)


### Features

* **race:** publish pit entry and exit (pit.entered, pit.exited) ([#28](https://github.com/those-who-run/ecu/issues/28)) ([474076e](https://github.com/those-who-run/ecu/commit/474076e247696e4f1bd65a3e3b26fc43b820fd44))
* **race:** publish the session's kind (session.kind) ([#27](https://github.com/those-who-run/ecu/issues/27)) ([ebb4430](https://github.com/those-who-run/ecu/commit/ebb44307fa2a9ab111b5f1881add88a0f537cd39))

## [0.1.4](https://github.com/those-who-run/ecu/compare/v0.1.3...v0.1.4) (2026-10-29)


### Features

* **race:** publish the session's track (session.track) ([#24](https://github.com/those-who-run/ecu/issues/24)) ([e379fe5](https://github.com/those-who-run/ecu/commit/e379fe54273422a42a57ce3ac2fde0943c9c6900))
* **replay:** replay a v1 recording onto the bus (ecu replay) ([#23](https://github.com/those-who-run/ecu/issues/23)) ([64e9055](https://github.com/those-who-run/ecu/commit/64e905545424bbbb11770556c26c84051bb1b3ea))

## [0.1.3](https://github.com/those-who-run/ecu/compare/v0.1.2...v0.1.3) (2026-10-28)


### Features

* **live:** listen to the PS5 and record each drive in v1's format ([#18](https://github.com/those-who-run/ecu/issues/18)) ([88a8e0a](https://github.com/those-who-run/ecu/commit/88a8e0a76fcda63b94398d27031238dc731a9f40))

## [0.1.2](https://github.com/those-who-run/ecu/compare/v0.1.1...v0.1.2) (2026-10-28)


### Features

* **track:** the track table and v1's track detector, as a pure library ([#17](https://github.com/those-who-run/ecu/issues/17)) ([380588c](https://github.com/those-who-run/ecu/commit/380588c0245b1ce4e45541ef26273bfec4ca3755))


### Bug Fixes

* **bus:** cut a stalled connection when Close's time runs out ([#19](https://github.com/those-who-run/ecu/issues/19)) ([fdc1d5e](https://github.com/those-who-run/ecu/commit/fdc1d5e4c1bc0e6f1123d147a53e345772dbdee9))

## [0.1.1](https://github.com/those-who-run/ecu/compare/v0.1.0...v0.1.1) (2026-10-28)


### Features

* **bus:** publish frames and the three events on NATS ([#12](https://github.com/those-who-run/ecu/issues/12)) ([7a5e3db](https://github.com/those-who-run/ecu/commit/7a5e3db0b429633c88428d290731eb8fab3e5cb4))
* **race:** session start, laps and session end as a pure state machine ([#11](https://github.com/those-who-run/ecu/issues/11)) ([39111d6](https://github.com/those-who-run/ecu/commit/39111d6e056f7c3515f37c808d6477941ade9fa0))

## 0.1.0 (2026-10-26)


### Bug Fixes

* **service:** check the error the server returns after shutdown ([#7](https://github.com/those-who-run/ecu/issues/7)) ([5b24a79](https://github.com/those-who-run/ecu/commit/5b24a79fec27ca6ed0a6568627953868c1c64d6d))
