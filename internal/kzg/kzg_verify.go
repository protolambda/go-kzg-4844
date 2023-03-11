package kzg

import (
	"math/big"

	"github.com/consensys/gnark-crypto/ecc"
	bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
	"github.com/crate-crypto/go-proto-danksharding-crypto/internal/utils"
)

// Proof to the claim that a polynomial f(X) was evaluated at a point `z` and
// resulted in `f(z)`
type OpeningProof struct {
	// H quotient polynomial (f(X) - f(z))/(X-z)
	QuotientComm bls12381.G1Affine

	// Point that we are evaluating the polynomial at : `z`
	InputPoint fr.Element

	// ClaimedValue purported value : `f(z)`
	ClaimedValue fr.Element
}

// Verify a single KZG proof.
//
// Returns `nil` if verification was successful, an error otherwise.
//
// If one wants to check if the verification failed due to the pairings
// check, one can check for `ErrVerifyOpeningProof`.
//
// # Copied and modified from gnark-crypto
//
// [verify_kzg_proof_impl](https://github.com/ethereum/consensus-specs/blob/3a2304981a3b820a22b518fe4859f4bba0ebc83b/specs/deneb/polynomial-commitments.md#verify_kzg_proof_impl)
func Verify(commitment *Commitment, proof *OpeningProof, openKey *OpeningKey) error {
	// [-1]G₂
	// It's possible to precompute this, however Negation
	// is cheap (2 Fp negations), so doing it per verify
	// should be insignificant compared to the rest of Verify.
	var negG2 bls12381.G2Affine
	negG2.Neg(&openKey.GenG2)

	// Convert the G2 generator to Jacobian for
	// later computations.
	var genG2Jac bls12381.G2Jac
	genG2Jac.FromAffine(&openKey.GenG2)

	// This has been changed slightly from the way that gnark-crypto
	// does it to show the symmetry in the computation required for
	// G₂ and G₁. This is the way it is done in the specs.

	// In the specs, this is denoted as `X_minus_z`
	//
	// [z]G₂
	var inputPointG2Jac bls12381.G2Jac
	var pointBigInt big.Int
	proof.InputPoint.BigInt(&pointBigInt)
	inputPointG2Jac.ScalarMultiplication(&genG2Jac, &pointBigInt)

	// [α - z]G₂
	var alphaMinusZG2Jac bls12381.G2Jac
	alphaMinusZG2Jac.FromAffine(&openKey.AlphaG2)
	alphaMinusZG2Jac.SubAssign(&inputPointG2Jac)

	// [α-z]G₂ (Convert to Affine format)
	var alphaMinusZG2Aff bls12381.G2Affine
	alphaMinusZG2Aff.FromJacobian(&alphaMinusZG2Jac)

	//  In the specs, this is denoted as `P_minus_y`
	//
	// [f(z)]G₁
	var claimedValueG1Aff bls12381.G1Jac
	var claimedValueBigInt big.Int
	proof.ClaimedValue.BigInt(&claimedValueBigInt)
	claimedValueG1Aff.ScalarMultiplicationAffine(&openKey.GenG1, &claimedValueBigInt)

	// [f(α) - f(z)]G₁
	var fminusfzG1Jac bls12381.G1Jac
	fminusfzG1Jac.FromAffine(commitment)
	fminusfzG1Jac.SubAssign(&claimedValueG1Aff)

	// [f(α) - f(z)]G₁ (Convert to Affine format)
	var fminusfzG1Aff bls12381.G1Affine
	fminusfzG1Aff.FromJacobian(&fminusfzG1Jac)

	check, err := bls12381.PairingCheck(
		[]bls12381.G1Affine{fminusfzG1Aff, proof.QuotientComm},
		[]bls12381.G2Affine{negG2, alphaMinusZG2Aff},
	)
	if err != nil {
		return err
	}
	if !check {
		return ErrVerifyOpeningProof
	}

	return nil
}

// Verifies `N` KZG proofs in a batch.
//
// - This method is more efficient than calling Verify `N` times.
// - Randomness is used to combine multiple proofs into one.
//
// # Copied and modified from gnark-crypto
//
// [verify_kzg_proof_batch](https://github.com/ethereum/consensus-specs/blob/3a2304981a3b820a22b518fe4859f4bba0ebc83b/specs/deneb/polynomial-commitments.md#verify_kzg_proof_batch)
func BatchVerifyMultiPoints(commitments []Commitment, proofs []OpeningProof, openKey *OpeningKey) error {
	// Check consistency number of proofs is equal to the number of commitments.
	if len(commitments) != len(proofs) {
		return ErrInvalidNbDigests
	}

	// If there is nothing to verify, we return nil
	// to signal that verification was true.
	//
	// TODO: upstream change to gnark repo
	if len(commitments) == 0 {
		return nil
	}

	// if only one commitment, call Verify
	if len(commitments) == 1 {
		return Verify(&commitments[0], &proofs[0], openKey)
	}

	// sample random numbers for sampling
	// We only need to sample one random number and
	// compute powers of that random number. This works
	// since powers will produce a vandermonde matrix
	// which is linearly independent.
	var randomNumber fr.Element
	_, err := randomNumber.SetRandom()
	if err != nil {
		return err
	}
	randomNumbers := utils.ComputePowers(randomNumber, uint(len(commitments)))

	// combine random_i*quotient_i
	var foldedQuotients bls12381.G1Affine
	quotients := make([]bls12381.G1Affine, len(proofs))
	for i := 0; i < len(randomNumbers); i++ {
		quotients[i].Set(&proofs[i].QuotientComm)
	}
	config := ecc.MultiExpConfig{}
	_, err = foldedQuotients.MultiExp(quotients, randomNumbers, config)
	if err != nil {
		return nil
	}

	// fold commitments and evals
	evals := make([]fr.Element, len(commitments))
	for i := 0; i < len(randomNumbers); i++ {
		evals[i].Set(&proofs[i].ClaimedValue)
	}
	foldedCommitments, foldedEvals, err := fold(commitments, evals, randomNumbers)
	if err != nil {
		return err
	}

	// compute commitment to folded Eval
	var foldedEvalsCommit bls12381.G1Affine
	var foldedEvalsBigInt big.Int
	foldedEvals.BigInt(&foldedEvalsBigInt)
	foldedEvalsCommit.ScalarMultiplication(&openKey.GenG1, &foldedEvalsBigInt)

	// compute F = foldedCommitments - foldedEvalsCommit
	foldedCommitments.Sub(&foldedCommitments, &foldedEvalsCommit)

	// combine random_i*(point_i*quotient_i)
	var foldedPointsQuotients bls12381.G1Affine
	for i := 0; i < len(randomNumbers); i++ {
		randomNumbers[i].Mul(&randomNumbers[i], &proofs[i].InputPoint)
	}
	_, err = foldedPointsQuotients.MultiExp(quotients, randomNumbers, config)
	if err != nil {
		return err
	}

	// lhs first pairing
	foldedCommitments.Add(&foldedCommitments, &foldedPointsQuotients)

	// lhs second pairing
	foldedQuotients.Neg(&foldedQuotients)

	// pairing check
	check, err := bls12381.PairingCheck(
		[]bls12381.G1Affine{foldedCommitments, foldedQuotients},
		[]bls12381.G2Affine{openKey.GenG2, openKey.AlphaG2},
	)
	if err != nil {
		return err
	}
	if !check {
		return ErrVerifyOpeningProof
	}

	return nil
}

// Copied from gnark-crypto
func fold(commitments []Commitment, evaluations []fr.Element, factors []fr.Element) (Commitment, fr.Element, error) {
	// length inconsistency between commitments and evaluations should have been done before calling this function
	nbCommitments := len(commitments)

	// fold the claimed values
	var foldedEvaluations, tmp fr.Element
	for i := 0; i < nbCommitments; i++ {
		tmp.Mul(&evaluations[i], &factors[i])
		foldedEvaluations.Add(&foldedEvaluations, &tmp)
	}

	// fold the commitments
	var foldedCommitments Commitment
	_, err := foldedCommitments.MultiExp(commitments, factors, ecc.MultiExpConfig{})
	if err != nil {
		return foldedCommitments, foldedEvaluations, err
	}

	// folding done
	return foldedCommitments, foldedEvaluations, nil
}
