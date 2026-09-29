package pggo

import (
	"testing"
)

func TestSCRAMVector(t *testing.T) {
	// RFC 7677 test vector (user "user", password "pencil").
	s := &scram{password: "pencil", nonce: "rOprNGfwEbeRWgbNEkqO"}
	s.clientFirst()
	s.clientBare = "n=user,r=rOprNGfwEbeRWgbNEkqO"
	final, err := s.clientFinal([]byte("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"))
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if string(final) != want {
		t.Fatalf("got %s", final)
	}
	if !s.verifyServer([]byte("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")) {
		t.Fatal("server signature not verified")
	}
}
