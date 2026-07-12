function check(a,b){if(a!==b)throw a+" !== "+b;} check(parseInt('0777'),777);check(parseInt('010'),10);console.log('PASS');
