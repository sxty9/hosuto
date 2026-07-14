// Package rights enumerates the fine-grained rights this service declares to the holistic
// rights standard. Each constant is the Linux group backing one permission in
// permissions/hosuto.json — keep the two in sync. Enforcement uses auth.User.Can, i.e. the
// standard rule: isAdmin || group ∈ groups.
package rights

const (
	// GroupPlay lets a member link their game account, reach the servers they were added to
	// and download the client export. Default-on: without it hosuto does nothing for a member.
	GroupPlay = "hp_hosuto_play"

	// GroupHost lets a member create and own servers. Default-off: a server commits real
	// resources — RAM, a port from the pool and a public domain.
	GroupHost = "hp_hosuto_host"

	// GroupAdmin lets a member see and control every server on the host, not only their own.
	GroupAdmin = "hp_hosuto_admin"
)
