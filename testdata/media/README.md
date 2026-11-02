# Shared media fixtures (CANT-312)

One fixture set for the image-upload verdict, read by three tests: the Go ingest test in `cmd/catenary/mediafixtures_test.go`, and the TypeScript and Dart pre-check tests (CANT-314, CANT-317). `manifest.json` lists every file with the verdict the server gives it and the image cap; each test asserts the same verdicts, so a client's supported-type list or size cap that drifts from the server's fails a build.

The oversize case is not a file. Each test builds it from the manifest's `oversize` entry: the header bytes in `headerHex`, then `padByte` up to `imageCapBytes + 1` bytes in all. No binary over 1 MiB enters the repository. The Go test fails if `imageCapBytes` and `DefaultMaxImageBytes` disagree.

This directory is outside `schema/` and is edited by hand.

`messages.json` holds client-side attach wording that both clients show and must keep identical (CANT-360): the several-files refusal, `oneImage`. The web test asserts `ONE_IMAGE_MESSAGE` equals it and the Dart test asserts `oneImageMessage` equals it, so changing the string in one runtime fails the other. The wrong-type and too-large refusals are not here: they are the server's own words, carried by the upload messages.
