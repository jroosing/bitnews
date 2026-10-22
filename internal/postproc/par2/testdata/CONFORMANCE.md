# Conformance findings (M3-06, par2cmdline 1.4.0)

Our parser (M3-02), Verify (M3-03), gf16 (M3-04) and Repair (M3-05) were
compared with par2cmdline 1.4.0 on the 12 golden cases and 146 damage
scenarios. Result: **no deviation in the PAR2 format handling**. Every point
the task asked about matches:

- Packet header, lengths (multiples of 4), MD5, set ID = MD5(Main body): match.
  Packets tile every `.par2` file exactly, and no packet of an unknown type
  appears.
- Main packet: slice size, file IDs, no non-recovery files. FileDesc: name
  (spaces, unicode), length, full MD5, 16 KiB MD5, file ID formula
  MD5(MD5-16k, length LE64, name). IFSC: MD5 and CRC32 of each slice, the last
  one zero padded. All equal values recomputed from the payload.
- RecvSlic: exponent and 4-byte-aligned data tail of the packet, one slice
  size of data; exponents equal those announced in the `volNN+MM` file names,
  including offsets (`-f7`) and exponents above 32767.
- Creator packet: present ("Created by par2cmdline version 1.4.0.").
- **gf16: every recovery slice par2cmdline wrote is byte-identical to
  `InputConstants` + `Pow` + `MulAdd` over the payload** in global input order
  (so exclusion of multiples of 3, 5, 17 and 257, the generator polynomial,
  little-endian words and the zero padding of the last slice are right). The
  `many_slices` case (650 slices) is the one that exercises the 257 rule;
  deleting `|| e%257 == 0` from `InputConstants` makes it fail (mutation
  checked), and deleting the 17 rule fails every case with 17 or more slices.
- Repair of 1 slice, several slices, the full recovery capacity, whole
  deleted files, truncated files, trailing garbage, renamed files: byte-exact
  with the real recovery volumes, also from a single volume with exponents
  that do not start at 0, and with a 1000 byte chunk. Damage beyond capacity
  gives `InsufficientError{Needed, Available}`. With `PAR2_CONFORMANCE=1`,
  real `par2 r` repairs exactly the same scenarios and refuses the same ones,
  and `par2 v` accepts the directory our Repair produced.

## Observations (not bugs in our code)

1. **File ID order is little-endian numeric.** The Main packet lists the
   recovery-set files sorted as 128-bit *little-endian* integers (the last
   byte is the most significant), not as byte strings. This is also the order
   of the input slices. `Set.FileIDs` keeps the Main packet order, so Verify
   and Repair are right; anyone re-sorting IDs with `bytes.Compare` would get
   the constants wrong. (`goldenCompareID` in `golden_test.go`.)
2. **Empty files are not part of a set.** par2cmdline prints "Skipping 0 byte
   file" and writes no description for it. Verify therefore reports nothing
   for such a file.
3. **Trailing bytes on a file shorter than one slice.** After appending bytes,
   Verify reports the file `Damaged` with 0 damaged slices for every file
   (a short last slice is matched by its own length), and so does par2cmdline
   for files of at least one slice. For a file *shorter than one slice*,
   par2cmdline cannot match its only slice and counts one block lost, where we
   need 0 recovery slices (Repair truncates). Both end with the correct file;
   ours is not a bug. The comparison test allows exactly this difference.
4. par2cmdline 1.4.0 rejects `-f` above about 32768 ("Invalid first block
   option"), so exponents near 65535 cannot be produced with it.
5. Volume files start with a RecvSlic packet, then repeat Main/FileDesc/IFSC
   between recovery packets, and end with Creator; Scan handles this order.
