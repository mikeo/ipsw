package download

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/blacktop/go-plist"
	"github.com/blacktop/ipsw/internal/download/pcc"
	"github.com/blacktop/ipsw/internal/utils"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const bagURL = "https://init-kt-prod.ess.apple.com/init/getBag?ix=5&p=atresearch"

type BagResponse struct {
	UUID                          string `plist:"uuid,omitempty"`
	AtResearcherConsistencyProof  string `plist:"at-researcher-consistency-proof,omitempty"`
	AtResearcherListTrees         string `plist:"at-researcher-list-trees,omitempty"`
	AtResearcherLogHead           string `plist:"at-researcher-log-head,omitempty"`
	AtResearcherLogInclusionProof string `plist:"at-researcher-log-inclusion-proof,omitempty"`
	AtResearcherLogLeaves         string `plist:"at-researcher-log-leaves,omitempty"`
	AtResearcherPublicKeys        string `plist:"at-researcher-public-keys,omitempty"`
	BagExpiryTimestamp            int    `plist:"bag-expiry-timestamp,omitempty"`
	BagType                       string `plist:"bag-type,omitempty"`
	BuildVersion                  string `plist:"build-version,omitempty"`
	Platform                      string `plist:"platform,omitempty"`
	TtrEnabled                    int    `plist:"ttr-enabled,omitempty"`
}

type TransparencyExtension struct {
	Type uint32
	Size uint16
	Data []byte
}

type ATLeaf struct {
	Version         uint8
	Type            uint8
	DescriptionSize uint8
	Description     []byte
	HashSize        uint8
	Hash            []byte
	ExpiryMS        int64
	ExtensionsSize  uint16
	Extensions      []TransparencyExtension
}

type Ticket struct {
	Raw            asn1.RawContent
	Version        int
	ApTicket       asn1.RawValue
	CryptexTickets []asn1.RawValue `asn1:"set"`
}

type PCCRelease struct {
	Index uint64
	pcc.ReleaseMetadata
	Ticket
	*ATLeaf
}

func (r PCCRelease) String() string {
	var out string
	out += fmt.Sprintf("Index:     %d\n", r.Index)
	out += fmt.Sprintf("Type:      %s\n", pcc.ATLogDataType(r.Type).String())
	out += fmt.Sprintf("Timestamp: %s\n", time.Time(r.GetTimestamp().AsTime()).Format(time.RFC3339))
	out += fmt.Sprintf("Expires:   %s\n", time.UnixMilli(r.ExpiryMS).Format(time.RFC3339))
	out += fmt.Sprintf("Hash:      %s\n", hex.EncodeToString(r.GetReleaseHash()))
	out += "Assets:\n"
	for _, asset := range r.GetAssets() {
		out += fmt.Sprintf("  Type:    %s\n", asset.GetType().String())
		out += fmt.Sprintf("    Variant: %s\n", asset.GetVariant())
		out += fmt.Sprintf("    Digest:  (%s) %s\n", strings.TrimPrefix(asset.Digest.GetDigestAlg().String(), "DIGEST_ALG_"), hex.EncodeToString(asset.Digest.GetValue()))
		out += fmt.Sprintf("    URL:     %s\n", asset.GetUrl())
	}
	out += "Tickets:\n"
	hash := sha256.New()
	hash.Write(r.Ticket.ApTicket.Bytes)
	out += fmt.Sprintf("  ApTicket: %s\n", hex.EncodeToString(hash.Sum(nil)))
	out += "  Cryptexes:\n"
	for i, ct := range r.Ticket.CryptexTickets {
		hash.Reset()
		hash.Write(ct.Bytes)
		out += fmt.Sprintf("    %d) %s\n", i, hex.EncodeToString(hash.Sum(nil)))
	}
	out += "DarwinInit:\n"
	dat, _ := json.MarshalIndent(r.DarwinInit.AsMap(), "", "  ")
	out += string(dat)
	return out
}

func parseAtLeaf(r *bytes.Reader) (*ATLeaf, error) {
	var leaf ATLeaf

	if err := binary.Read(r, binary.BigEndian, &leaf.Version); err != nil {
		return nil, fmt.Errorf("cannot read version: %v", err)
	}
	if err := binary.Read(r, binary.BigEndian, &leaf.Type); err != nil {
		return nil, fmt.Errorf("cannot read type: %v", err)
	}
	if err := binary.Read(r, binary.BigEndian, &leaf.DescriptionSize); err != nil {
		return nil, fmt.Errorf("cannot read description size: %v", err)
	}
	leaf.Description = make([]byte, leaf.DescriptionSize)
	if err := binary.Read(r, binary.BigEndian, &leaf.Description); err != nil {
		return nil, fmt.Errorf("cannot read description: %v", err)
	}
	if err := binary.Read(r, binary.BigEndian, &leaf.HashSize); err != nil {
		return nil, fmt.Errorf("cannot read hash size: %v", err)
	}
	leaf.Hash = make([]byte, leaf.HashSize)
	if err := binary.Read(r, binary.BigEndian, &leaf.Hash); err != nil {
		return nil, fmt.Errorf("cannot read hash: %v", err)
	}
	if err := binary.Read(r, binary.BigEndian, &leaf.ExpiryMS); err != nil {
		return nil, fmt.Errorf("cannot read expiry: %v", err)
	}
	if err := binary.Read(r, binary.BigEndian, &leaf.ExtensionsSize); err != nil {
		return nil, fmt.Errorf("cannot read extensions size: %v", err)
	}
	for i := 0; i < int(leaf.ExtensionsSize); i++ {
		var ext TransparencyExtension
		if err := binary.Read(r, binary.BigEndian, &ext.Type); err != nil {
			return nil, fmt.Errorf("cannot read extension type: %v", err)
		}
		if err := binary.Read(r, binary.BigEndian, &ext.Size); err != nil {
			return nil, fmt.Errorf("cannot read extension size: %v", err)
		}
		ext.Data = make([]byte, ext.Size)
		if err := binary.Read(r, binary.BigEndian, &ext.Data); err != nil {
			return nil, fmt.Errorf("cannot read extension data: %v", err)
		}
		leaf.Extensions = append(leaf.Extensions, ext)
	}
	return &leaf, nil
}

func GetPCCReleases(proxy string) ([]PCCRelease, error) {
	var releases []PCCRelease

	res, err := http.Get(bagURL)
	if err != nil {
		return nil, fmt.Errorf("failed to GET bag: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bag GET returned status: %s", res.Status)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	res.Body.Close()

	os.WriteFile("bag.plist", body, 0644)

	var bag BagResponse
	if _, err := plist.Unmarshal(body, &bag); err != nil {
		return nil, fmt.Errorf("cannot unmarshal plist: %v", err)
	}

	uuid := uuid.NewString()

	data, err := proto.Marshal(&pcc.ListTreesRequest{
		Version:     pcc.ProtocolVersion_V3,
		RequestUuid: uuid,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot marshal ListTreesRequest: %v", err)
	}

	req, err := http.NewRequest("POST", bag.AtResearcherListTrees, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot create http POST request: %v", err)
	}
	req.Header.Set("X-Apple-Request-UUID", uuid)
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Add("User-Agent", utils.RandomAgent())

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           GetProxy(proxy),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	res, err = client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("returned status: %s", res.Status)
	}

	body, err = io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	res.Body.Close()

	var lt pcc.ListTreesResponse
	if err := proto.Unmarshal(body, &lt); err != nil {
		return nil, fmt.Errorf("cannot unmarshal ListTreesResponse: %v", err)
	}

	var tree *pcc.ListTreesResponse_Tree
	for _, t := range lt.GetTrees() {
		if t.GetLogType() == pcc.LogType_AT_LOG &&
			t.GetApplication() == pcc.Application_PRIVATE_CLOUD_COMPUTE {
			tree = t
		}
	}

	data, err = proto.Marshal(&pcc.LogHeadRequest{
		Version:     pcc.ProtocolVersion_V3,
		TreeId:      tree.GetTreeId(),
		Revision:    -1,
		RequestUuid: uuid,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot marshal ListTreesRequest: %v", err)
	}

	req, err = http.NewRequest("POST", bag.AtResearcherLogHead, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot create http POST request: %v", err)
	}
	req.Header.Set("X-Apple-Request-UUID", uuid)
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Add("User-Agent", utils.RandomAgent())

	res, err = client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("returned status: %s", res.Status)
	}

	body, err = io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	res.Body.Close()

	var lh pcc.LogHeadResponse
	if err := proto.Unmarshal(body, &lh); err != nil {
		return nil, fmt.Errorf("cannot unmarshal ListTreesResponse: %v", err)
	}
	var logHead pcc.LogHead
	if err := proto.Unmarshal(lh.GetLogHead().GetObject(), &logHead); err != nil {
		return nil, fmt.Errorf("cannot unmarshal LogHead: %v", err)
	}

	data, err = proto.Marshal(&pcc.LogLeavesRequest{
		Version:         pcc.ProtocolVersion_V3,
		TreeId:          tree.GetTreeId(),
		StartIndex:      0,
		EndIndex:        logHead.GetLogSize(),
		RequestUuid:     uuid,
		StartMergeGroup: 0,
		EndMergeGroup:   uint32(tree.GetMergeGroups()),
	})
	if err != nil {
		return nil, fmt.Errorf("cannot marshal ListTreesRequest: %v", err)
	}

	req, err = http.NewRequest("POST", bag.AtResearcherLogLeaves, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot create http POST request: %v", err)
	}
	req.Header.Set("X-Apple-Request-UUID", uuid)
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Add("User-Agent", utils.RandomAgent())

	res, err = client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("returned status: %s", res.Status)
	}

	body, err = io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	res.Body.Close()

	var lls pcc.LogLeavesResponse
	if err := proto.Unmarshal(body, &lls); err != nil {
		return nil, fmt.Errorf("cannot unmarshal ListTreesResponse: %v", err)
	}

	for _, leave := range lls.GetLeaves() {
		if leave.GetNodeType() == pcc.NodeType_ATL_NODE {
			var clnode pcc.ChangeLogNodeV2
			if err := proto.Unmarshal(leave.GetNodeBytes(), &clnode); err != nil {
				return nil, fmt.Errorf("cannot unmarshal ChangeLogNodeV2: %v", err)
			}
			// peak at the first 2 bytes to see if it's a release
			blob := make([]byte, 2)
			if err := binary.Read(bytes.NewReader(clnode.GetMutation()), binary.BigEndian, &blob); err != nil {
				return nil, fmt.Errorf("cannot read type: %v", err)
			}
			if pcc.ATLogDataType(blob[1]) != pcc.ATLogDataType_RELEASE {
				continue
			}
			release := PCCRelease{Index: leave.GetIndex()}
			release.ATLeaf, err = parseAtLeaf(bytes.NewReader(clnode.GetMutation()))
			if err != nil {
				return nil, fmt.Errorf("cannot parse ATLeaf: %v", err)
			}
			if err := proto.Unmarshal(leave.GetMetadata(), &release.ReleaseMetadata); err != nil {
				return nil, fmt.Errorf("cannot unmarshal ReleaseMetadata: %v", err)
			}
			if _, err := asn1.Unmarshal(leave.RawData, &release.Ticket); err != nil {
				return nil, fmt.Errorf("failed to ASN.1 parse Img4: %v", err)
			}
			releases = append(releases, release)
		}
	}

	return releases, nil
}
