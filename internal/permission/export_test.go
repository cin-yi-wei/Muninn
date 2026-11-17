package permission

import "net"

func netDial(socket string) (net.Conn, error) { return net.Dial("unix", socket) }
