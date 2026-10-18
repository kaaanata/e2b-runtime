//go:build linux

package fc

// maxcpusParam boots a CPU-hotplug guest with only the template's CPUs
// online. Measured on the fork FC with the 6.1 guest kernel: with
// vcpu_count=32 and maxcpus=4 the guest reports present/possible 0-31, online
// 0-3, and every other CPU can be brought online at runtime (~60 ms each);
// nr_cpus=4 instead shrinks possible to 0-3 and they never can.
const maxcpusParam = "maxcpus"
