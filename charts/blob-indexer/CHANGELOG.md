# Changelog

## [0.2.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.1.0...blob-indexer-chart-v0.2.0) (2026-05-24)


### Features

* add Helm chart integration tests with ct and kind ([#60](https://github.com/a-thomas-22/blob-indexer-api/issues/60)) ([d44e474](https://github.com/a-thomas-22/blob-indexer-api/commit/d44e4747af08a43d7cf444602465cc19f37832ae))
* split indexer and API into separate applications ([e0651e2](https://github.com/a-thomas-22/blob-indexer-api/commit/e0651e234f884369bb6cde3e63b5579c48911320))
* split indexer and API into separate applications ([7266f5c](https://github.com/a-thomas-22/blob-indexer-api/commit/7266f5c6eece2ffbac9e7677f5544535091fb408))


### Bug Fixes

* **helm:** add restrictive pod and container security contexts ([#72](https://github.com/a-thomas-22/blob-indexer-api/issues/72)) ([c521b46](https://github.com/a-thomas-22/blob-indexer-api/commit/c521b466e666a3bbbf982e1f637e8140b6b51ee0))
* **helm:** add service account and database network policy ([#73](https://github.com/a-thomas-22/blob-indexer-api/issues/73)) ([ef84b18](https://github.com/a-thomas-22/blob-indexer-api/commit/ef84b182288517d472af4b5a931e462ebc4c4590))
* **security:** make DB sslmode configurable and enforce in non-dev ([#68](https://github.com/a-thomas-22/blob-indexer-api/issues/68)) ([27ef8aa](https://github.com/a-thomas-22/blob-indexer-api/commit/27ef8aae4fc75a7910aecd40fe483019c5d5f16a))
* **security:** move DB URL from ConfigMap to Secret ([#67](https://github.com/a-thomas-22/blob-indexer-api/issues/67)) ([6b92539](https://github.com/a-thomas-22/blob-indexer-api/commit/6b92539636a3c73630b99eb0928f424921239e95))
