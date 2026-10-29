#!/usr/bin/env python3
# Host side of the chaos verifier. Reads the verifier disk and checks that
# the blocks written by run RUN form a contiguous valid prefix, that at most
# one block after that prefix is torn, and that nothing of this run exists
# beyond it. Prints a one line summary; exits 0 on success.
import struct, sys, zlib

MAGIC = b"NOXVRFY1"
BS = 4096

def main():
    dev, run = sys.argv[1], int(sys.argv[2])
    valid = 0
    torn = None
    stray = []
    with open(dev, "rb", buffering=0) as f:
        seq = 0
        while True:
            b = f.read(BS)
            if len(b) < BS:
                break
            hdr = b[:24]
            if hdr[:8] != MAGIC:
                if any(b):
                    pass  # other run or garbage: ignore
                seq += 1
                continue
            brun, bseq = struct.unpack("<QQ", hdr[8:24])
            if brun != run:
                seq += 1
                continue
            crc_ok = zlib.crc32(b[:BS - 4]) & 0xffffffff == struct.unpack("<I", b[BS - 4:])[0] and bseq == seq
            if crc_ok and torn is None and seq == valid:
                valid += 1
            elif torn is None and seq == valid:
                torn = seq
            else:
                stray.append(seq)
            seq += 1
            if torn is not None and len(stray) > 8:
                break
    status = "OK" if not stray else "FAIL"
    print(f"{status} run={run} valid_prefix={valid} torn_block={torn} stray={stray[:8]}")
    sys.exit(0 if not stray else 1)

if __name__ == "__main__":
    main()
