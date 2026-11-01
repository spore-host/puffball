"""puff local rung: run an OpenQASM 3 file on the Braket default simulator.

Embedded in the puff binary and run as
    uv run --no-project --with amazon-braket-default-simulator==<pin> python -I puff_local.py <file.qasm> --shots N
Prints one JSON object on stdout: counts, the shots actually measured,
measured qubits, and the version of every package in the simulator's closure.
Needs no AWS credentials and makes no network call once uv has the package.
Refuses to report a result if an AWS SDK was loaded (A-offline, #9).
"""

import argparse
import collections
import importlib.metadata
import json
import platform
import sys
import time

# The simulator's dependency closure whose versions a result records. Only
# the simulator is pinned; the rest can change between online runs.
CLOSURE = (
    "amazon-braket-default-simulator",
    "amazon-braket-schemas",
    "numpy",
    "scipy",
    "sympy",
    "pydantic",
    "numba",
    "antlr4-python3-runtime",
    "opt-einsum",
)

AWS_SDKS = ("boto3", "botocore", "aiobotocore", "s3transfer")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("program", help="OpenQASM 3 source file")
    parser.add_argument("--shots", type=int, default=1000)
    args = parser.parse_args()

    from braket.default_simulator import StateVectorSimulator
    from braket.ir.openqasm import Program

    with open(args.program, encoding="utf-8") as f:
        source = f.read()

    start = time.perf_counter()
    result = StateVectorSimulator().run_openqasm(Program(source=source, inputs={}), shots=args.shots)
    elapsed = time.perf_counter() - start

    loaded = sorted(m for m in AWS_SDKS if m in sys.modules)
    if loaded:
        print(f"puff_local: an AWS SDK was loaded ({', '.join(loaded)}); refusing to report", file=sys.stderr)
        return 3

    counts = collections.Counter("".join(str(bit) for bit in m) for m in result.measurements)
    software = {"python": platform.python_version()}
    missing = []
    for dist in CLOSURE:
        try:
            software[dist] = importlib.metadata.version(dist)
        except importlib.metadata.PackageNotFoundError:
            missing.append(f"software.{dist}")
    missing.append("rng_seed")  # puff neither sets nor records one

    json.dump(
        {
            "device": "braket_sv (local)",
            "shots": len(result.measurements),
            "measured_qubits": list(result.measuredQubits),
            "counts": dict(sorted(counts.items())),
            "simulate_seconds": round(elapsed, 4),
            "software": software,
            "missing": missing,
        },
        sys.stdout,
        indent=2,
    )
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
