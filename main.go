package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/blakesmith/ar"
)

type Conf struct {
	ListenPort   string   `json:"listenPort"`
	RootRepoPath string   `json:"rootRepoPath"`
	SupportArch  []string `json:"supportedArch"`
	EnableSSL    bool     `json:"enableSSL"`
	SSLCert      string   `json:"SSLcert"`
	SSLKey       string   `json:"SSLkey"`
}

func (c Conf) ArchPath(arch string) string {
	return filepath.Join(c.RootRepoPath, "/dists/stable/main/binary-"+arch)
}

type DeleteObj struct {
	Filename string
	Arch     string
}

var (
	mutex        sync.Mutex
	configFile   = flag.String("c", "conf.json", "config file location")
	parsedConfig = Conf{}
)

func main() {
	flag.Parse()
	file, err := ioutil.ReadFile(*configFile)
	if err != nil {
		log.Fatal("unable to read config file, exiting...")
	}
	//config = &Conf{}
	if err := json.Unmarshal(file, &parsedConfig); err != nil {
		log.Fatal("unable to marshal config file, exiting...")
	}

	if err := createDirs(parsedConfig); err != nil {
		log.Println(err)
		log.Fatalf("error creating directory structure, exiting")
	}
	http.Handle("/", http.StripPrefix("/", http.FileServer(http.Dir(parsedConfig.RootRepoPath))))
	http.Handle("/upload", uploadHandler(parsedConfig))
	http.Handle("/delete", deleteHandler(parsedConfig))
	if parsedConfig.EnableSSL {
		log.Println("running with SSL enabled")
		log.Fatal(http.ListenAndServeTLS(":"+parsedConfig.ListenPort, parsedConfig.SSLCert, parsedConfig.SSLKey, nil))
	} else {
		log.Println("running without SSL enabled")
		log.Fatal(http.ListenAndServe(":"+parsedConfig.ListenPort, nil))
	}
}

func createDirs(config Conf) error {
	for _, arch := range config.SupportArch {
		if _, err := os.Stat(config.ArchPath(arch)); err != nil {
			if os.IsNotExist(err) {
				log.Printf("Directory for %s does not exist, creating", arch)
				dirErr := os.MkdirAll(config.ArchPath(arch), 0755)
				if dirErr != nil {
					return fmt.Errorf("error creating directory for %s: %s", arch, dirErr)
				}
			} else {
				return fmt.Errorf("error inspecting %s: %s", arch, err)
			}
		}
	}
	return nil
}

func inspectPackage(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("error opening package file %s: %s", filename, err)
	}

	arReader := ar.NewReader(f)
	defer f.Close()
	var controlBuf bytes.Buffer

	for {
		header, err := arReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return "", fmt.Errorf("error in inspectPackage loop: %s", err)
		}

		if header.Name == "control.tar.gz" {
			io.Copy(&controlBuf, arReader)
			return inspectPackageControl(controlBuf)

		}

	}
	return "", nil
}

func inspectPackageControl(filename bytes.Buffer) (string, error) {
	gzf, err := gzip.NewReader(bytes.NewReader(filename.Bytes()))
	if err != nil {
		return "", fmt.Errorf("error creating gzip reader: %s", err)
	}

	tarReader := tar.NewReader(gzf)
	var controlBuf bytes.Buffer
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return "", fmt.Errorf("failed to inpect package: %s", err)
		}

		name := header.Name

		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if name == "./control" {
				io.Copy(&controlBuf, tarReader)
				return controlBuf.String(), nil
			}
		default:
			log.Printf("%s : %c %s %s\n",
				"Unable to figure out type",
				header.Typeflag,
				"in file",
				name,
			)
		}
	}
	return "", nil
}

func createPackagesGz(config Conf, arch string) error {
	outfile, err := os.Create(filepath.Join(config.ArchPath(arch), "Packages.gz"))
	if err != nil {
		return fmt.Errorf("failed to create packages.gz: %s", err)
	}
	defer outfile.Close()
	gzOut := gzip.NewWriter(outfile)
	defer gzOut.Close()
	// loop through each directory
	// run inspectPackage
	dirList, err := ioutil.ReadDir(config.ArchPath(arch))
	if err != nil {
		return fmt.Errorf("scanning: %s: %s", config.ArchPath(arch), err)
	}
	for _, debFile := range dirList {
		if strings.HasSuffix(debFile.Name(), "deb") {
			var packBuf bytes.Buffer
			debPath := filepath.Join(config.ArchPath(arch), debFile.Name())
			tempCtlData, err := inspectPackage(debPath)
			if err != nil {
				return err
			}
			packBuf.WriteString(tempCtlData)
			dir := filepath.Join("dists/stable/main/binary-"+arch, debFile.Name())
			fmt.Fprintf(&packBuf, "Filename: %s\n", dir)
			fmt.Fprintf(&packBuf, "Size: %d\n", debFile.Size())
			f, err := os.Open(debPath)
			if err != nil {
				log.Println("error opening deb file: ", err)
			}
			defer f.Close()

			var (
				md5hash    = md5.New()
				sha1hash   = sha1.New()
				sha256hash = sha256.New()
			)
			_, err = io.Copy(io.MultiWriter(md5hash, sha1hash, sha256hash), f)
			if err != nil {
				log.Println("error with the md5 hash: ", err)
			}
			fmt.Fprintf(&packBuf, "MD5sum: %s\n",
				hex.EncodeToString(md5hash.Sum(nil)))
			if _, err = io.Copy(sha1hash, f); err != nil {
				log.Println("error with the sha1 hash: ", err)
			}
			fmt.Fprintf(&packBuf, "SHA1: %s\n",
				hex.EncodeToString(sha1hash.Sum(nil)))
			if _, err = io.Copy(sha256hash, f); err != nil {
				log.Println("error with the sha256 hash: ", err)
			}
			fmt.Fprintf(&packBuf, "SHA256: %s\n",
				hex.EncodeToString(sha256hash.Sum(nil)))
			packBuf.WriteString("\n\n")
			gzOut.Write(packBuf.Bytes())
			f = nil
		}
	}

	gzOut.Flush()
	return nil
}

func uploadHandler(config Conf) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			log.Println("not a POST")
			return
		}
		queryVals := r.URL.Query()
		archType := "all"
		if queryVals.Get("arch") != "" {
			archType = queryVals.Get("arch")
		}
		reader, err := r.MultipartReader()
		if err != nil {
			httpErrorf(w, "error creating multipart reader: %s", err)
			return
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				log.Println("breaking")
				break
			}
			log.Println("form part: ", part.FormName())
			if part.FileName() == "" {
				continue
			}
			log.Println("found file: ", part.FileName())
			log.Println("creating files: ", part.FileName())
			dst, err := os.Create(filepath.Join(config.ArchPath(archType), part.FileName()))
			if err != nil {
				httpErrorf(w, "error creating deb file: %s", err)
				return
			}
			defer dst.Close()
			if _, err := io.Copy(dst, part); err != nil {
				httpErrorf(w, "error writing deb file: %s", err)
				return
			}
		}

		log.Println("grabbing lock...")
		mutex.Lock()
		defer mutex.Unlock()

		log.Println("got lock, updating package list...")
		createPkgRes := createPackagesGz(config, archType)
		if err := createPkgRes; err != nil {
			httpErrorf(w, "error creating package: %s", err)
			return
		}
		log.Println("lock returned")
		w.WriteHeader(http.StatusOK)
	})
}

func deleteHandler(config Conf) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			log.Println("not a DELETE")
			return
		}
		var toDelete DeleteObj
		if err := json.NewDecoder(r.Body).Decode(&toDelete); err != nil {
			httpErrorf(w, "failed to decode json: %s", err)
			return
		}
		debPath := filepath.Join(config.ArchPath(toDelete.Arch), toDelete.Filename)
		if err := os.Remove(debPath); err != nil {
			httpErrorf(w, "failed to delete: %s", err)
			return
		}
		log.Println("grabbing lock...")
		mutex.Lock()
		defer mutex.Unlock()

		log.Println("got lock, updating package list...")
		if err := createPackagesGz(config, toDelete.Arch); err != nil {
			httpErrorf(w, "failed to create package: %s", err)
			return
		}
		log.Println("lock returned")
	})
}

func httpErrorf(w http.ResponseWriter, format string, a ...interface{}) {
	err := fmt.Errorf(format, a...)
	log.Println(err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
