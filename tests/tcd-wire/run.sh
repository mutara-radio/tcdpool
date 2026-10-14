#!/bin/sh
# Runs the tcd link tests inside the tcd-test image.
set -u
fail=0
echo "== vectors: generated from the unpatched struct must equal the committed file"
genvectors > /tmp/vectors.txt
if cmp -s /tmp/vectors.txt /src/testdata/tcd-wire-vectors.txt; then echo "PASS vectors match"; else echo "FAIL vectors differ"; diff /tmp/vectors.txt /src/testdata/tcd-wire-vectors.txt | head -5; fail=1; fi
echo "== codec_test (patched TCPacketCodec + TCFraming)"
codec_test /src/testdata/tcd-wire-vectors.txt || fail=1
echo "== md380_state: do the AMBE+2 encoder and decoder share state?"
md380_state || fail=1
echo "== wiretest: patched tcd, raw framing"
wiretest /usr/local/bin/tcd raw || fail=1
echo "== wiretest: patched tcd, v2 framing"
wiretest /usr/local/bin/tcd v2 || fail=1
echo "== wiretest: UNPATCHED tcd, raw framing (failures here are expected; they show what the patches fix)"
wiretest /usr/local/bin/tcd-upstream raw
echo "== result: $([ $fail = 0 ] && echo PASS || echo FAIL) (patched tcd)"
exit $fail
