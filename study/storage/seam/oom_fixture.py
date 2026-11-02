"""CI-only allocation fixture: bounded at 512 MiB, expected cap is 128 MiB."""
chunks = []
for _ in range(64):
    chunk = bytearray(8 * 1024 ** 2)
    # Touch every page; demand-zero virtual memory alone is not OOM evidence.
    chunk[::4096] = b"x" * (len(chunk) // 4096)
    chunks.append(chunk)
raise RuntimeError("OOM fixture reached its allocation bound without OOM")
