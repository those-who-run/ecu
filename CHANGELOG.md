# Changelog

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
