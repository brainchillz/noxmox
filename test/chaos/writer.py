#!/usr/bin/env python3
# Guest side of the chaos verifier. Writes sequence numbered 4 KiB blocks to
# a raw disk, fsyncing after every block, until killed. Each block carries
# the run id, its sequence number and a CRC, so the host side checker can
# tell which blocks were acknowledged before a crash.
import os, signal, struct, sys, time, zlib

MAGIC = b"NOXVRFY1"
BS = 4096

def main():
    dev, run = sys.argv[1], int(sys.argv[2])
    fd = os.open(dev, os.O_WRONLY | os.O_DIRECT if hasattr(os, "O_DIRECT") else os.O_WRONLY)
    size = os.lseek(fd, 0, os.SEEK_END)
    os.lseek(fd, 0, os.SEEK_SET)
    nblocks = size // BS
    seq = 0
    t0 = time.time()
    stop = []
    signal.signal(signal.SIGINT, lambda *a: stop.append(1))
    signal.signal(signal.SIGTERM, lambda *a: stop.append(1))
    import mmap
    buf = mmap.mmap(-1, BS)
    while seq < nblocks and not stop:
        payload = (struct.pack("<I", seq) * ((BS - 32) // 4))
        hdr = MAGIC + struct.pack("<QQ", run, seq)
        body = hdr + payload
        crc = zlib.crc32(body) & 0xffffffff
        rec = body + struct.pack("<I", crc)
        rec += b"\0" * (BS - len(rec))
        buf.seek(0); buf.write(rec)
        os.pwrite(fd, buf, seq * BS)
        os.fsync(fd)
        seq += 1
        if seq % 256 == 0:
            print(f"acked {seq} blocks in {time.time()-t0:.1f}s", flush=True)
    print(f"acked {seq} blocks in {time.time()-t0:.1f}s, stopped", flush=True)

if __name__ == "__main__":
    main()
