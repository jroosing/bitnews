# PAR2 golden fixtures

Sets written by the real **par2cmdline 1.4.0** (Homebrew, `par2 --version`
prints `par2cmdline version 1.4.0`), used by `golden_test.go`,
`golden_repair_test.go` and the benchmarks in `internal/postproc/par2`.
par2cmdline ran only offline, to produce these files (ADR-0002); the product
never runs it.

## Layout

```
golden/<case>/data/        payload files (seeded pseudo-random bytes)
golden/<case>/par2/        what `par2 c` wrote (set.par2, set.volNN+MM.par2)
golden/<case>/manifest.json
```

`manifest.json` holds the expected values: file names, sizes, MD5s, whether the
file is part of the set, slice size, recovery block count, the recovery
exponents, and a list of damage scenarios (byte flips, truncation, deletion,
rename, appended bytes) with the expected Verify verdict per file and the
number of damaged slices. It is written by
`internal/postproc/par2/internal/fixturegen` from the payload bytes, from the
block size and recovery block count that par2cmdline prints, and from the
volume file names. It is never derived from our own parser, so a test
failure is a conformance bug in our code.

## Cases

| case | par2 arguments | what it covers |
|---|---|---|
| single_small | `-s4096 -r50` | one file, 3 slices |
| multi_files | `-s4096 -r10 -n2` | 5 files: not a slice multiple, shorter than one slice, exact multiple, and an empty file (par2cmdline skips it) |
| slice_16k | `-s16384 -r40 -n3` | larger slices, a 1 byte file |
| block_count | `-b64 -r15` | par2cmdline picks the slice size (5984) |
| explicit_uniform | `-s4096 -c20 -u -n4` | explicit recovery count, uniform volumes |
| variable_volumes | `-s2048 -r20 -n5` | default (growing) volume sizes |
| limited_volumes | `-s4096 -c20 -l` | volume size limit, many volumes |
| first_block | `-s4096 -c10 -f7 -n2` | exponents start at 7 |
| high_exponents | `-s4096 -c10 -f32764 -n2` | exponents 32764..32773 (par2cmdline 1.4.0 rejects larger `-f`) |
| unicode_names | `-s4096 -c4` | spaces, double spaces, `ß`, CJK and parentheses in file names |
| many_slices | `-s32 -c8` | 650 input slices, so the input constants skip multiples of 3, 5, 17 and 257 |
| large | `-s16384 -r10 -n3` | 1.4 MB, 90 slices, the benchmark case |

(The script is the authority for arguments; each manifest records them.)

## Regenerating

```sh
scripts/gen-par2-fixtures.sh              # all cases
scripts/gen-par2-fixtures.sh unicode_names large   # some cases
git status internal/postproc/par2/testdata          # a rerun must change nothing
```

Needs `par2` on PATH and Go. Payload bytes come from
`go run ./internal/postproc/par2/internal/fixturegen payload -seed N`
(math/rand/v2 PCG, fixed seed per file), never from `/dev/urandom`.
Regenerating with another par2cmdline version may change the `.par2` bytes
(creator text, packet order); update this file when it does. Keep the total
under 5 MiB (currently about 3.9 MB).

## Running the conformance tests

```sh
go test ./internal/postproc/par2/                 # needs no external program
PAR2_CONFORMANCE=1 go test -run Golden ./internal/postproc/par2/
                                                  # also runs real `par2 v` / `par2 r`
PAR2_CONFORMANCE=1 go test -tags bigbench -run '^$' -bench Big -benchtime 1x ./internal/postproc/par2/
                                                  # 512 MiB benchmark, files made in a temp dir
```

`CONFORMANCE.md` lists what the comparison found.
