OPENQASM 3.0;
bit[2] b;
qubit[2] q;
h q[0];
cnot q[0], q[1];
b[0] = measure q[0];
b[1] = measure q[1];
