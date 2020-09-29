# Egress Filtering Benchmark

This repository contains a set of tools to measure the egress filtering performance using BPF, iptables, ipsets and calico.

## How to Use

1. Setup two computers to run the test. You need to have Docker, iptables and ipset installed and you should be able to connect to those computers with SSH without requiring a password.

2. Create a Kubernetes cluster using [Lokomotive](https://github.com/kinvolk/lokomotive) with atleast one worker node.
   Label the worker node as follows:
   ```
   kubectl k label node <node_name> nodetype=worker-benchmark
   ```

   Set location of kubeconfig using the environment variable *KUBECONFIG*:

   ```
   export KUBECONFIG=<location-to-kubeconfig>
   ```

2. Configure the parameters of the test in the [parameters.py](benchmark/parameters.py) file.

3. Install the required libraries in the client to run the Python script

```
pip install -r requirements.txt
```

4. Execute the tests:

```
$ cd benchmark
$ make
$ python run_tests.py --mode udp --username USERNAME --client CLIENTADDR --server SERVERADDR
```

This will create some csv files with the information about the test.
You can plot them by your self or follow the next step.

5. Plot the data by running

```
$ python plot_data.py
```

This will create some svg files with the graphs.

## Credits

The BPF filter is inspired by the [tc-bpf](https://man7.org/linux/man-pages/man8/tc-bpf.8.html) man page and the [Cilium documentation](https://docs.cilium.io/en/latest/bpf/#tc-traffic-control).
