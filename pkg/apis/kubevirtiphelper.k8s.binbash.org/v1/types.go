package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPReservationSpec   `json:"spec,omitempty"`
	Status IPReservationStatus `json:"status,omitempty"`
}

type IPReservationSpec struct {
	VMName         string           `json:"vmname,omitempty"`
	IPReservations []IPReservations `json:"ipreservations,omitempty"`
}

type IPReservations struct {
	IPAddress   string `json:"ipaddress,omitempty"`
	MACAddress  string `json:"macaddress,omitempty"`
	NetworkName string `json:"networkname,omitempty"`
}

type IPReservationStatus struct {
	Name string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPReservationList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// List of Fips.
	Items []IPReservation `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPPoolSpec   `json:"spec,omitempty"`
	Status IPPoolStatus `json:"status,omitempty"`
}

type IPPoolSpec struct {
	CIDR        string `json:"cidr,omitempty"`
	NetworkName string `json:"networkname,omitempty"`
}

type IPPoolStatus struct {
	Name string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// List of Fips.
	Items []IPPool `json:"items"`
}
