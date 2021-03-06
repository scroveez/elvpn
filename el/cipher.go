/*
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 * Author: Mahmoud Abdelsalam <scroveez@gmail.com>
 *
 */

package el

import (
	"bytes"
	"crypto/aes"
	_cipher "crypto/cipher"
	"crypto/rand"

	"github.com/golang/snappy"
)

type elCipher struct {
	block _cipher.Block
}

const cipherBlockSize = 16

func newElCipher(key []byte) (*elCipher, error) {
	s := new(elCipher)
	key = PKCS5Padding(key, cipherBlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	s.block = block
	return s, nil
}

func (s *elCipher) encrypt(msg []byte) []byte {
	cmsg := make([]byte, snappy.MaxEncodedLen(len(msg)))
	cmsg = snappy.Encode(cmsg, msg)

	pmsg := PKCS5Padding(cmsg, cipherBlockSize)
	buf := make([]byte, len(pmsg)+cipherBlockSize)

	iv := buf[:cipherBlockSize]
	rand.Read(iv)
	encrypter := _cipher.NewCBCEncrypter(s.block, iv)
	encrypter.CryptBlocks(buf[cipherBlockSize:], pmsg)

	return buf
}

func (s *elCipher) decrypt(iv []byte, ctext []byte) []byte {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("%v", err)
		}
	}()
	decrypter := _cipher.NewCBCDecrypter(s.block, iv)
	buf := make([]byte, len(ctext))
	decrypter.CryptBlocks(buf, ctext)
	cmsg := PKCS5UnPadding(buf)

	msg, _ := snappy.Decode(nil, cmsg)
	return msg
}

func PKCS5Padding(ciphertext []byte, blockSize int) []byte {
	padding := blockSize - len(ciphertext)%blockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(ciphertext, padtext...)
}

func PKCS5UnPadding(origData []byte) []byte {
	length := len(origData)
	unpadding := int(origData[length-1])
	return origData[:(length - unpadding)]
}
