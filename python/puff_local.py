"""puff local rung: run an OpenQASM 3 file on the Braket default simulator.

Embedded in the puff binary and run as
    uv run --with amazon-braket-default-simulator==<pin> python puff_local.py <file.qasm> --shots N
Prints one JSON object on stdout: counts, shots, measured qubits, versions.
Needs no AWS credentials and makes no network call once uv has the package.
"""

import argparse
import collections
import json
import platform
import sys
import time


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("program", help="OpenQASM 3 source file")
    parser.add_argument("--shots", type=int, default=1000)
    args = parser.parse_args()

    from braket.default_simulator import StateVectorSimulator
    from braket.default_simulator import __version__ as simulator_version
    from braket.ir.openqasm import Program

    with open(args.program, encoding="utf-8") as f:
        source = f.read()

    start = time.perf_counter()
    result = StateVectorSimulator().run_openqasm(Program(source=source, inputs={}), shots=args.shots)
    elapsed = time.perf_counter() - start

    counts = collections.Counter("".join(str(bit) for bit in m) for m in result.measurements)
    json.dump(
        {
            "device": "braket_sv (local)",
            "shots": args.shots,
            "measured_qubits": list(result.measuredQubits),
            "counts": dict(sorted(counts.items())),
            "simulate_seconds": round(elapsed, 4),
            "software": {
                "amazon-braket-default-simulator": simulator_version,
                "python": platform.python_version(),
            },
        },
        sys.stdout,
        indent=2,
    )
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
