function check(a,b){if(a!==b)throw a+" !== "+b;} var r=new RegExp('abc','g');check(r.source,'abc');check(r.global,true);check(r.test('xabcy'),true);console.log('PASS');
